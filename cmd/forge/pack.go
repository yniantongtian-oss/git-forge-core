package main

// Packfile reader and writer: all four object types, OFS_DELTA and REF_DELTA (thin packs resolve against the
// repository's own objects), trailer verification, and pack assembly from lazily deflated, cached streams.
//
// Speed:
//   - Parsing reuses ONE pooled raw-deflate reader per pack (flate.Resetter.Reset per entry); the zlib header and
//     Adler-32 trailer are checked directly. It reads straight from the request buffer through an io.ByteReader,
//     so flate consumes exactly the entry's compressed bytes (no bufio read-ahead) and the read position is the
//     trailer's offset. Delta payloads inflate into one reusable scratch buffer; object ids reuse one SHA-1 digest.
//   - A whole (non-delta) object received by push keeps its verified zlib stream, so serving it costs no deflate.
//     Writing a pack compresses only objects without one (pooled zlib writers, level 1, fanned out over
//     GOMAXPROCS for big batches) and caches each stream on its object: later packs are concatenation plus SHA-1.

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"hash"
	"hash/adler32"
	"io"
	"math/bits"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

// PackError is a malformed or unusable pack; receive-pack reports its text as the unpack status.
type PackError string

func (e PackError) Error() string { return string(e) }

// ---------------------------------------------------------------- deltas

// ApplyDelta applies a git delta (copy/insert instructions) to its base.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readSize := func() (uint64, error) {
		var result uint64
		var shift uint
		for {
			if pos >= len(delta) {
				return 0, PackError("truncated delta header")
			}
			b := delta[pos]
			pos++
			if shift < 64 {
				result |= uint64(b&0x7f) << shift
			}
			shift += 7
			if b&0x80 == 0 {
				return result, nil
			}
		}
	}
	baseSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if baseSize != uint64(len(base)) {
		return nil, PackError("delta base size mismatch")
	}
	size64, err := readSize()
	if err != nil {
		return nil, err
	}
	// Every delta byte yields at most 64 KiB of output; a larger claim can never be satisfied.
	if size64 > uint64(len(delta))*0x10000 {
		return nil, PackError("delta result size mismatch")
	}
	resultSize := int(size64)
	out := make([]byte, resultSize)
	written := 0
	for pos < len(delta) {
		op := delta[pos]
		pos++
		switch {
		case op&0x80 != 0:
			if pos+bits.OnesCount8(op&0x7f) > len(delta) {
				return nil, PackError("delta copy out of range")
			}
			offset, size := 0, 0
			if op&0x01 != 0 {
				offset = int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				offset |= int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				offset |= int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				offset |= int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				size = int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				size |= int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				size |= int(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) || written+size > resultSize {
				return nil, PackError("delta copy out of range")
			}
			copy(out[written:], base[offset:offset+size])
			written += size
		case op != 0:
			n := int(op)
			if pos+n > len(delta) || written+n > resultSize {
				return nil, PackError("delta insert out of range")
			}
			copy(out[written:], delta[pos:pos+n])
			pos += n
			written += n
		default:
			return nil, PackError("delta opcode 0")
		}
	}
	if written != resultSize {
		return nil, PackError("delta result size mismatch")
	}
	return out, nil
}

// ---------------------------------------------------------------- inflating

// packSource feeds a pack's bytes to the zlib reader. Being an io.ByteReader (flate.Reader), flate pulls it
// byte by byte and never past the end of the current stream, so pos ends exactly after the Adler-32 trailer.
type packSource struct {
	buf []byte
	pos int
}

func (s *packSource) Read(p []byte) (int, error) {
	if s.pos >= len(s.buf) {
		return 0, io.EOF
	}
	n := copy(p, s.buf[s.pos:])
	s.pos += n
	return n, nil
}

func (s *packSource) ReadByte() (byte, error) {
	if s.pos >= len(s.buf) {
		return 0, io.EOF
	}
	b := s.buf[s.pos]
	s.pos++
	return b, nil
}

// inflater bundles a raw-deflate reader with the source it reads, so the pooled pair never pins a finished pack.
// The 2-byte zlib header and the Adler-32 trailer are checked here directly: compress/zlib would allocate a fresh
// Adler-32 state for every object and hash the output incrementally through an interface.
type inflater struct {
	src packSource
	fr  io.ReadCloser // a compress/flate reader, Reset for every stream
}

var inflaterPool = sync.Pool{New: func() any { return new(inflater) }}

var (
	errZlibHeader     = errors.New("zlib: invalid header")
	errZlibDictionary = errors.New("zlib: invalid dictionary")
	errZlibChecksum   = errors.New("zlib: invalid checksum")
)

// reset validates the RFC 1950 header at buf[pos] (deflate, window <= 32 KiB, FCHECK, no preset dictionary) and
// points the raw-deflate reader just past it.
func (f *inflater) reset(buf []byte, pos int) error {
	if pos+2 > len(buf) {
		return io.ErrUnexpectedEOF
	}
	cmf, flg := buf[pos], buf[pos+1]
	if cmf&0x0f != 8 || cmf>>4 > 7 || (uint16(cmf)<<8|uint16(flg))%31 != 0 {
		return errZlibHeader
	}
	if flg&0x20 != 0 {
		return errZlibDictionary
	}
	f.src.buf, f.src.pos = buf, pos+2
	if f.fr == nil {
		f.fr = flate.NewReader(&f.src)
		return nil
	}
	return f.fr.(flate.Resetter).Reset(&f.src, nil)
}

func (f *inflater) release() {
	f.src.buf, f.src.pos = nil, 0
	inflaterPool.Put(f)
}

// ---------------------------------------------------------------- parsing

type pendingRefDelta struct {
	baseOID string
	delta   []byte
	start   int
}

type packParser struct {
	data    []byte // the pack without its 20-byte trailer
	inf     *inflater
	digest  hash.Hash
	head    [32]byte
	scratch []byte
	objects []*GitObject
	offsets []int                 // entry offset of objects[i]; ascending during the main pass
	inPack  map[string]*GitObject // by id, built only once a REF_DELTA needs it
}

// digestPool recycles the SHA-1 state ParsePack hashes object ids with.
var digestPool = sync.Pool{New: func() any { return sha1.New() }}

var parserPool = sync.Pool{New: func() any {
	return &packParser{
		objects: make([]*GitObject, 0, 16),
		offsets: make([]int, 0, 16),
	}
}}

func badZlib(start int, err error) error {
	return PackError("bad zlib stream at " + strconv.Itoa(start) + ": " + err.Error())
}

// inflate decompresses the zlib stream at p.data[pos:] into out, which must come out exactly full, verifies its
// Adler-32 trailer in one pass over out, and returns the offset just past the stream.
func (p *packParser) inflate(pos, start int, out []byte) (int, error) {
	if err := p.inf.reset(p.data, pos); err != nil {
		return 0, badZlib(start, err)
	}
	fr := p.inf.fr
	filled, ended := 0, false
	for filled < len(out) {
		n, err := fr.Read(out[filled:])
		filled += n
		if err == io.EOF {
			if filled < len(out) {
				return 0, PackError("entry at " + strconv.Itoa(start) + " inflates to " + strconv.Itoa(filled) + " bytes, header says " + strconv.Itoa(len(out)))
			}
			ended = true
			break
		}
		if err != nil {
			return 0, badZlib(start, err)
		}
	}
	// The output is full: the deflate stream must end right here (its end-of-block code).
	for probe := [1]byte{}; !ended; {
		n, err := fr.Read(probe[:])
		if n > 0 {
			return 0, PackError("bad zlib stream at " + strconv.Itoa(start) + ": inflates past the " + strconv.Itoa(len(out)) + " bytes its header says")
		}
		if err == io.EOF {
			ended = true
		} else if err != nil {
			return 0, badZlib(start, err)
		}
	}
	// flate stops exactly at the end of the deflate data; the big-endian Adler-32 of the output follows.
	end := p.inf.src.pos
	if end+4 > len(p.data) {
		return 0, badZlib(start, io.ErrUnexpectedEOF)
	}
	if adler32.Checksum(out) != binary.BigEndian.Uint32(p.data[end:]) {
		return 0, badZlib(start, errZlibChecksum)
	}
	return end + 4, nil
}

func (p *packParser) scratchOf(n int) []byte {
	if cap(p.scratch) < n {
		p.scratch = make([]byte, n)
	}
	p.scratch = p.scratch[:n]
	return p.scratch
}

// object hashes data with the parse's one reusable digest. The deflated stream is left empty (lazy deflate).
func (p *packParser) object(t ObjType, data []byte) *GitObject {
	p.digest.Reset()
	p.digest.Write(appendObjectHeader(p.head[:0], t, len(data)))
	p.digest.Write(data)
	var sum [sha1.Size]byte
	p.digest.Sum(sum[:0])
	return &GitObject{OID: oidHex(&sum), Type: t, Data: data}
}

func (p *packParser) remember(start int, obj *GitObject) {
	p.objects = append(p.objects, obj)
	p.offsets = append(p.offsets, start)
	if p.inPack != nil {
		p.inPack[obj.OID] = obj
	}
}

// atOffset is the object whose entry starts at offset (OFS_DELTA bases; offsets are ascending in the main pass).
func (p *packParser) atOffset(offset int) *GitObject {
	if i := sort.SearchInts(p.offsets, offset); i < len(p.offsets) && p.offsets[i] == offset {
		return p.objects[i]
	}
	return nil
}

// byID finds a REF_DELTA base in the pack (later duplicates win), then in the repository.
func (p *packParser) byID(oid string, external func(string) *GitObject) *GitObject {
	if p.inPack == nil {
		p.inPack = make(map[string]*GitObject, len(p.objects)+16)
		for _, obj := range p.objects {
			p.inPack[obj.OID] = obj
		}
	}
	if obj := p.inPack[oid]; obj != nil {
		return obj
	}
	if external != nil {
		return external(oid)
	}
	return nil
}

// ParsePack parses a whole pack (version 2 or 3) after verifying its trailing SHA-1. external supplies bases for
// thin-pack REF_DELTAs (it may be nil); bases inside the pack win. Objects come back in pack order, with
// REF_DELTAs whose base came later appended as they resolve. The buffer is only read, never retained.
func ParsePack(buf []byte, external func(oid string) *GitObject) ([]*GitObject, error) {
	if len(buf) < 32 || string(buf[:4]) != "PACK" {
		return nil, PackError("bad pack signature")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != 2 && version != 3 {
		return nil, PackError("unsupported pack version " + strconv.FormatUint(uint64(version), 10))
	}
	count := binary.BigEndian.Uint32(buf[8:12])
	end := len(buf) - 20
	if sha1.Sum(buf[:end]) != [sha1.Size]byte(buf[end:]) {
		return nil, PackError("pack checksum mismatch")
	}

	// Every entry takes at least ~10 bytes, which bounds how much to presize for a lying count.
	hint := int(min(uint64(count), uint64(end/10)+1))
	p := parserPool.Get().(*packParser)
	p.data = buf[:end]
	p.inf = inflaterPool.Get().(*inflater)
	p.digest = digestPool.Get().(hash.Hash)
	if cap(p.objects) < hint {
		p.objects = make([]*GitObject, 0, hint)
		p.offsets = make([]int, 0, hint)
	} else {
		p.objects = p.objects[:0]
		p.offsets = p.offsets[:0]
	}
	p.inPack = nil
	defer func() {
		p.inf.release()
		digestPool.Put(p.digest)
		clear(p.objects)
		p.data = nil
		p.inf = nil
		p.digest = nil
		p.inPack = nil
		if cap(p.scratch) > 4<<20 {
			p.scratch = nil
		}
		if cap(p.objects) > 4096 {
			p.objects = nil
			p.offsets = nil
		}
		parserPool.Put(p)
	}()
	lookup := func(oid string) *GitObject { return p.byID(oid, external) }

	// Copy the compressed body once so every non-delta object keeps its exact verified zlib stream without
	// pinning the whole HTTP request buffer or re-deflating on fetch/clone.
	var zSlab []byte
	if end > 12 {
		zSlab = bytes.Clone(buf[12:end])
	}

	var pending []pendingRefDelta
	pos := 12
	for index := uint32(0); index < count; index++ {
		start := pos
		if pos >= end {
			return nil, PackError("pack ends before its declared object count")
		}
		c := buf[pos]
		pos++
		typeCode := int(c>>4) & 7
		size := uint64(c & 15)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= end {
				return nil, PackError("truncated entry header")
			}
			c = buf[pos]
			pos++
			if shift < 64 {
				size |= uint64(c&0x7f) << shift
			}
			shift += 7
		}
		baseOffset := -1
		baseOID := ""
		switch {
		case typeCode == 6:
			if pos >= end {
				return nil, PackError("truncated ofs-delta header")
			}
			b := buf[pos]
			pos++
			distance := uint64(b & 0x7f)
			for b&0x80 != 0 {
				if pos >= end {
					return nil, PackError("truncated ofs-delta offset")
				}
				b = buf[pos]
				pos++
				distance = (distance+1)<<7 | uint64(b&0x7f)
			}
			if distance <= uint64(start) {
				baseOffset = start - int(distance)
			}
		case typeCode == 7:
			if pos+20 > end {
				return nil, PackError("truncated ref-delta header")
			}
			var raw [sha1.Size]byte
			copy(raw[:], buf[pos:pos+20])
			baseOID = oidHex(&raw)
			pos += 20
		case typeCode < 1 || typeCode > 4:
			return nil, PackError("invalid object type " + strconv.Itoa(typeCode))
		}
		// Deflate expands at most ~1032:1, so a bigger size claim cannot be honest; refuse before allocating it.
		if size > uint64(end-pos)*1032+64 {
			return nil, PackError("entry at " + strconv.Itoa(start) + " inflates to fewer bytes than the " + strconv.FormatUint(size, 10) + " its header says")
		}
		var err error
		if typeCode <= 4 {
			zlibStart := pos
			data := make([]byte, int(size))
			if pos, err = p.inflate(pos, start, data); err != nil {
				return nil, err
			}
			obj := p.object(ObjType(typeCode), data)
			if zSlab != nil && pos >= zlibStart {
				z := zSlab[zlibStart-12 : pos-12 : pos-12]
				obj.deflated.Store(&z)
			}
			p.remember(start, obj)
			continue
		}
		delta := p.scratchOf(int(size))
		if pos, err = p.inflate(pos, start, delta); err != nil {
			return nil, err
		}
		var base *GitObject
		if typeCode == 6 {
			if base = p.atOffset(baseOffset); base == nil {
				return nil, PackError("ofs-delta at " + strconv.Itoa(start) + " names an unknown base offset")
			}
		} else if base = lookup(baseOID); base == nil {
			pending = append(pending, pendingRefDelta{baseOID: baseOID, delta: bytes.Clone(delta), start: start})
			continue
		}
		result, err := ApplyDelta(base.Data, delta)
		if err != nil {
			return nil, err
		}
		p.remember(start, p.object(base.Type, result))
	}
	if pos != end {
		return nil, PackError("trailing bytes between the last entry and the checksum")
	}

	for len(pending) > 0 {
		var next []pendingRefDelta
		for _, item := range pending {
			base := lookup(item.baseOID)
			if base == nil {
				next = append(next, item)
				continue
			}
			result, err := ApplyDelta(base.Data, item.delta)
			if err != nil {
				return nil, err
			}
			p.remember(item.start, p.object(base.Type, result))
		}
		if len(next) == len(pending) {
			return nil, PackError("a ref-delta base is missing from the pack and the repository")
		}
		pending = next
	}
	out := make([]*GitObject, len(p.objects))
	copy(out, p.objects)
	return out, nil
}

// ---------------------------------------------------------------- writing

type deflater struct {
	buf bytes.Buffer
	zw  *zlib.Writer
}

var deflaterPool = sync.Pool{New: func() any {
	d := new(deflater)
	d.zw, _ = zlib.NewWriterLevel(&d.buf, zlib.BestSpeed)
	return d
}}

// deflateBytes compresses data into one zlib stream at level 1 with a pooled writer; the result is an exact-size copy.
func deflateBytes(data []byte) []byte {
	d := deflaterPool.Get().(*deflater)
	d.buf.Reset()
	d.zw.Reset(&d.buf)
	d.zw.Write(data) // writing into a bytes.Buffer cannot fail
	d.zw.Close()
	out := make([]byte, d.buf.Len())
	copy(out, d.buf.Bytes())
	if d.buf.Cap() > 4<<20 {
		d.buf = bytes.Buffer{} // do not pin a huge buffer in the pool
	}
	deflaterPool.Put(d)
	return out
}

// parallelDeflateMin is how many not-yet-deflated bytes a pack needs before compression fans out over cores.
const parallelDeflateMin = 256 << 10

// preparePack returns each object's deflated stream (compressing and caching the missing ones) and the exact
// size of the serialized pack.
func preparePack(objects []*GitObject) ([][]byte, int) {
	streams := make([][]byte, len(objects))
	var pending []int
	pendingBytes := 0
	for i, obj := range objects {
		if d := obj.deflated.Load(); d != nil {
			streams[i] = *d
		} else {
			pending = append(pending, i)
			pendingBytes += len(obj.Data)
		}
	}
	if len(pending) > 0 {
		workers := min(runtime.GOMAXPROCS(0), len(pending), pendingBytes/(64<<10)+1)
		if pendingBytes < parallelDeflateMin || workers < 2 {
			for _, i := range pending {
				streams[i] = objects[i].Deflated()
			}
		} else {
			var next atomic.Int64
			var wg sync.WaitGroup
			wg.Add(workers)
			for range workers {
				go func() {
					defer wg.Done()
					for {
						k := int(next.Add(1) - 1)
						if k >= len(pending) {
							return
						}
						i := pending[k]
						streams[i] = objects[i].Deflated()
					}
				}()
			}
			wg.Wait()
		}
	}
	size := 32
	for i, obj := range objects {
		size += entryHeaderLen(len(obj.Data)) + len(streams[i])
	}
	return streams, size
}

// emitPack streams the pack bytes through write: the "PACK" v2 header, each entry header plus its deflated
// stream, then the SHA-1 of everything before it.
func emitPack(objects []*GitObject, streams [][]byte, write func([]byte)) {
	digest := sha1.New()
	var head [12]byte
	copy(head[:], "PACK")
	binary.BigEndian.PutUint32(head[4:], 2)
	binary.BigEndian.PutUint32(head[8:], uint32(len(objects)))
	digest.Write(head[:])
	write(head[:])
	var entry [16]byte
	for i, obj := range objects {
		h := AppendEntryHeader(entry[:0], obj.Type, len(obj.Data))
		digest.Write(h)
		write(h)
		digest.Write(streams[i])
		write(streams[i])
	}
	var sum [sha1.Size]byte
	write(digest.Sum(sum[:0]))
}

// WritePack assembles a version-2 pack from objects in the given order.
func WritePack(objects []*GitObject) []byte {
	streams, size := preparePack(objects)
	out := make([]byte, 0, size)
	emitPack(objects, streams, func(b []byte) { out = append(out, b...) })
	return out
}
