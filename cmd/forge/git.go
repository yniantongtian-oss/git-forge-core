package main

// Git smart HTTP: ref advertisements, upload-pack (protocol v2 with a v0 fallback, shallow and deepen) and
// receive-pack v0 with report-status, side-band-64k, delete-refs and atomic. Fast-forward is enforced per ref.
//
// Every reply leaves HandleGit as ONE complete, pre-rendered HTTP/1.1 response (status line, headers, body)
// that the transport sends with a single conn.Write:
//   - the v2 /info/refs advertisement is a process-wide constant;
//   - v0 /info/refs (upload-pack and receive-pack) and ls-refs replies are cached per repository per RefsVersion;
//   - whole fetch replies for fresh clones (no haves, no shallow, no deepen-since, done) are cached by
//     (wants, depth) across repositories — the object graph is content-addressed — and concurrent identical
//     clones share one build (single flight): 50 simultaneous `git clone --depth 1` cost one pack build;
//   - bodies are assembled in place behind a reserved gap the HTTP head is written into, and packs stream
//     straight into side-band-64k framing, so a fetch reply never holds a second copy of its pack.
//
// Locking: HandleGit takes GitRepo.Mu itself (callers hold no lock). Fetch planning runs under RLock; pack
// writing (deflate, SHA-1) runs with no lock at all; receive-pack inflates and hashes the incoming pack before
// taking the write lock, which then covers only object insertion, verdicts and ref updates.

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	gitAgent = "forge-core/1.0"
	// SidebandChunk is the most payload one side-band-64k packet carries (65520 bytes with header and band).
	SidebandChunk = 65515
	maxGitBody    = 512 << 20
)

const (
	uploadCaps  = "side-band-64k thin-pack ofs-delta shallow deepen-since deepen-not deepen-relative no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want object-format=sha1"
	receiveCaps = "report-status delete-refs side-band-64k quiet atomic ofs-delta object-format=sha1"
)

const (
	ctPlainText     = "text/plain; charset=utf-8"
	ctUploadAdvert  = "application/x-git-upload-pack-advertisement"
	ctReceiveAdvert = "application/x-git-receive-pack-advertisement"
	ctUploadResult  = "application/x-git-upload-pack-result"
	ctReceiveResult = "application/x-git-receive-pack-result"
)

// GitNoCacheHeaders are the anti-caching header lines every git reply carries.
const GitNoCacheHeaders = "Cache-Control: no-cache, max-age=0, must-revalidate\r\nPragma: no-cache\r\nExpires: Fri, 01 Jan 1980 00:00:00 GMT\r\n"

// GitRequest is what HandleGit needs from one HTTP request under /{owner}/{repo}.git; the transport has already
// resolved the repository and read the whole body (de-chunked, at most 512 MiB).
type GitRequest struct {
	Method          string // "GET", "POST", ...
	Path            string // the path after ".git": "/info/refs", "/git-upload-pack", "/git-receive-pack"
	Query           string // the raw query string, without "?"
	GitProtocol     string // the Git-Protocol header, "" when absent
	ContentEncoding string // the Content-Encoding header, "" when absent
	Body            []byte // the raw body, still gzip-encoded when ContentEncoding says so
}

// HandleGitBytes answers a smart-HTTP request directly from raw byte slices on the hot path with zero string
// conversions or mutex contention on cached clone/push exchanges; anything unusual delegates to HandleGit.
func HandleGitBytes(g *GitRepo, method, path, query, gitProtocol, contentEncoding, body []byte) []byte {
	if string(method) == "GET" && string(path) == "/info/refs" {
		service := queryServiceBytes(query)
		if service == "" {
			return dumbHTTPFrame
		}
		if service == "git-upload-pack" && wantsV2Bytes(gitProtocol) {
			return v2AdvertFrame
		}
		return g.fastAdvertV0(service)
	}
	if string(method) == "POST" && len(contentEncoding) == 0 {
		if string(path) == "/git-upload-pack" && wantsV2Bytes(gitProtocol) {
			if frame := tryFastUploadV2(g, body); frame != nil {
				return frame
			}
		}
		if string(path) == "/git-receive-pack" {
			if frame := tryFastReceivePack(g, body); frame != nil {
				return frame
			}
		}
	}
	return HandleGit(g, &GitRequest{
		Method:          string(method),
		Path:            string(path),
		Query:           string(query),
		GitProtocol:     string(gitProtocol),
		ContentEncoding: string(contentEncoding),
		Body:            body,
	})
}

// HandleGit answers one smart-HTTP request and returns the complete HTTP/1.1 response bytes. The slice may be
// shared with other requests (cached replies): write it, never modify it.
func HandleGit(g *GitRepo, req *GitRequest) []byte {
	switch {
	case req.Path == "/info/refs" && req.Method == "GET":
		service := queryService(req.Query)
		if service == "" {
			return dumbHTTPFrame
		}
		if service == "git-upload-pack" && wantsV2(req.GitProtocol) {
			return v2AdvertFrame
		}
		return g.fastAdvertV0(service)

	case req.Path == "/git-upload-pack" && req.Method == "POST":
		body, err := decodeGitBody(req.Body, req.ContentEncoding)
		if err != nil {
			return gitBadRequestFrame
		}
		packets, _, err := parsePackets(body, 0, false)
		if err != nil {
			return gitPlainFrame(400, err.Error())
		}
		if !wantsV2(req.GitProtocol) {
			return fetchV0Frame(g, packets)
		}
		var args []string
		command, hasCommand := "", false
		inArgs := false
		for _, packet := range packets {
			if packet.kind == pktDelim {
				inArgs = true
				continue
			}
			if packet.kind == pktFlush {
				break
			}
			if packet.kind != pktData {
				continue
			}
			line := packetText(packet)
			if inArgs {
				args = append(args, line)
			} else if !hasCommand && strings.HasPrefix(line, "command=") {
				command, hasCommand = line, true
			}
		}
		switch command {
		case "command=ls-refs":
			return lsRefsFrame(g, args)
		case "command=fetch":
			return fetchV2Frame(g, args)
		}
		if !hasCommand {
			command = "(none)"
		}
		return uploadResultFrame(appendPkt(nil, "ERR unknown command ", command, "\n"))

	case req.Path == "/git-receive-pack" && req.Method == "POST":
		body, err := decodeGitBody(req.Body, req.ContentEncoding)
		if err != nil {
			return gitBadRequestFrame
		}
		out, err := receivePack(g, body)
		if err != nil {
			return gitPlainFrame(400, err.Error())
		}
		return HTTPFrame(200, ctReceiveResult, GitNoCacheHeaders, out)
	}
	return gitNotFoundFrame
}

// ---------------------------------------------------------------- HTTP/1.1 frames

// StatusText is the reason phrase Node's http module writes for a status code.
func StatusText(code int) string {
	switch code {
	case 100:
		return "Continue"
	case 101:
		return "Switching Protocols"
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 206:
		return "Partial Content"
	case 301:
		return "Moved Permanently"
	case 302:
		return "Found"
	case 303:
		return "See Other"
	case 304:
		return "Not Modified"
	case 307:
		return "Temporary Redirect"
	case 308:
		return "Permanent Redirect"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 406:
		return "Not Acceptable"
	case 408:
		return "Request Timeout"
	case 409:
		return "Conflict"
	case 410:
		return "Gone"
	case 411:
		return "Length Required"
	case 412:
		return "Precondition Failed"
	case 413:
		return "Payload Too Large"
	case 414:
		return "URI Too Long"
	case 415:
		return "Unsupported Media Type"
	case 422:
		return "Unprocessable Entity"
	case 429:
		return "Too Many Requests"
	case 431:
		return "Request Header Fields Too Large"
	case 500:
		return "Internal Server Error"
	case 501:
		return "Not Implemented"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	case 504:
		return "Gateway Timeout"
	case 505:
		return "HTTP Version Not Supported"
	}
	return "Unknown"
}

// AppendHTTPHead appends an HTTP/1.1 status line, Content-Type (when not empty), Content-Length (except for
// 1xx, 204 and 304), the extra header lines (each ending in "\r\n") and the blank line that ends the head.
func AppendHTTPHead(dst []byte, status int, contentType string, contentLength int, extra string) []byte {
	dst = append(dst, "HTTP/1.1 "...)
	dst = strconv.AppendInt(dst, int64(status), 10)
	dst = append(dst, ' ')
	dst = append(dst, StatusText(status)...)
	dst = append(dst, "\r\n"...)
	if contentType != "" {
		dst = append(dst, "Content-Type: "...)
		dst = append(dst, contentType...)
		dst = append(dst, "\r\n"...)
	}
	if status >= 200 && status != 204 && status != 304 {
		dst = append(dst, "Content-Length: "...)
		dst = strconv.AppendInt(dst, int64(contentLength), 10)
		dst = append(dst, "\r\n"...)
	}
	dst = append(dst, extra...)
	return append(dst, "\r\n"...)
}

// HTTPFrame renders a complete HTTP/1.1 response into one buffer.
func HTTPFrame(status int, contentType, extra string, body []byte) []byte {
	var scratch [256]byte
	head := AppendHTTPHead(scratch[:0], status, contentType, len(body), extra)
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	return append(out, body...)
}

// frameGap is the room newFrameBody leaves in front of a body for its HTTP head (a git head is ~210 bytes).
const frameGap = 256

// newFrameBody starts a response body that finishFrame later turns into a full frame without copying the body.
func newFrameBody(capHint int) []byte {
	return make([]byte, frameGap, frameGap+capHint)
}

// finishFrame writes the HTTP head into the gap in front of a body built from newFrameBody and returns the
// complete frame (capacity clipped, so it can be shared safely).
func finishFrame(buf []byte, status int, contentType, extra string) []byte {
	body := buf[frameGap:]
	var scratch [frameGap]byte
	head := AppendHTTPHead(scratch[:0], status, contentType, len(body), extra)
	if len(head) > frameGap {
		out := make([]byte, 0, len(head)+len(body))
		return append(append(out, head...), body...)
	}
	start := frameGap - len(head)
	copy(buf[start:frameGap], head)
	return buf[start:len(buf):len(buf)]
}

func gitPlainFrame(status int, message string) []byte {
	return HTTPFrame(status, ctPlainText, GitNoCacheHeaders, []byte(message+"\n"))
}

func uploadResultFrame(body []byte) []byte {
	return HTTPFrame(200, ctUploadResult, GitNoCacheHeaders, body)
}

var (
	v2AdvertFrame      = HTTPFrame(200, ctUploadAdvert, GitNoCacheHeaders, advertisementV2())
	dumbHTTPFrame      = gitPlainFrame(403, "Dumb HTTP transport is not supported; use the smart protocol (?service=git-upload-pack)")
	gitNotFoundFrame   = gitPlainFrame(404, "Not Found")
	gitBadRequestFrame = gitPlainFrame(400, "Bad request")
)

// ---------------------------------------------------------------- pkt-lines

const (
	hexDigits    = "0123456789abcdef"
	pktFlushLine = "0000"
	pktDelimLine = "0001"
)

func appendPktLen(dst []byte, n int) []byte {
	return append(dst, hexDigits[n>>12&15], hexDigits[n>>8&15], hexDigits[n>>4&15], hexDigits[n&15])
}

// appendPkt appends one pkt-line whose payload is the concatenation of parts.
func appendPkt(dst []byte, parts ...string) []byte {
	n := 4
	for _, part := range parts {
		n += len(part)
	}
	dst = appendPktLen(dst, n)
	for _, part := range parts {
		dst = append(dst, part...)
	}
	return dst
}

type pktKind uint8

const (
	pktData pktKind = iota
	pktFlush
	pktDelim
	pktEnd
)

type gitPacket struct {
	kind pktKind
	data []byte // aliases the request buffer
}

// GitProtocolError is a malformed request; HandleGit answers it with 400 and the message.
type GitProtocolError struct{ Msg string }

func (e *GitProtocolError) Error() string { return e.Msg }

// parsePackets parses pkt-lines from start; with stopAtFlush parsing ends after the first flush and the returned
// offset is where the data after it begins.
func parsePackets(buf []byte, start int, stopAtFlush bool) ([]gitPacket, int, error) {
	var packets []gitPacket
	pos := start
	for pos < len(buf) {
		if pos+4 > len(buf) {
			return nil, 0, &GitProtocolError{Msg: "truncated pkt-line length"}
		}
		length := 0
		for k := 0; k < 4; k++ {
			v, ok := hexNibble(buf[pos+k])
			if !ok {
				return nil, 0, &GitProtocolError{Msg: "bad pkt-line length " + jsQuote(latin1(buf[pos:pos+4]))}
			}
			length = length<<4 | int(v)
		}
		switch length {
		case 0:
			packets = append(packets, gitPacket{kind: pktFlush})
			pos += 4
			if stopAtFlush {
				return packets, pos, nil
			}
			continue
		case 1:
			packets = append(packets, gitPacket{kind: pktDelim})
			pos += 4
			continue
		case 2:
			packets = append(packets, gitPacket{kind: pktEnd})
			pos += 4
			continue
		}
		if length < 4 || pos+length > len(buf) {
			return nil, 0, &GitProtocolError{Msg: "bad pkt-line length"}
		}
		packets = append(packets, gitPacket{kind: pktData, data: buf[pos+4 : pos+length]})
		pos += length
	}
	return packets, pos, nil
}

// packetText is a data packet as text without one trailing newline ("" for flush/delim/end).
func packetText(packet gitPacket) string {
	if packet.kind != pktData {
		return ""
	}
	return strings.TrimSuffix(string(packet.data), "\n")
}

// appendSideband wraps data as side-band-64k packets on band (nothing for empty data).
func appendSideband(dst, data []byte, band byte) []byte {
	for offset := 0; offset < len(data); offset += SidebandChunk {
		chunk := data[offset:min(len(data), offset+SidebandChunk)]
		dst = appendPktLen(dst, len(chunk)+5)
		dst = append(dst, band)
		dst = append(dst, chunk...)
	}
	return dst
}

// sidebandFramer frames a byte stream of known total length as side-band packets while it is being written.
type sidebandFramer struct {
	out  []byte
	left int // stream bytes not framed yet
	room int // payload bytes left in the open packet
	band byte
}

func (f *sidebandFramer) write(p []byte) {
	for len(p) > 0 {
		if f.room == 0 {
			f.room = min(SidebandChunk, max(f.left, len(p)))
			f.out = appendPktLen(f.out, f.room+5)
			f.out = append(f.out, f.band)
		}
		k := min(len(p), f.room)
		f.out = append(f.out, p[:k]...)
		p = p[k:]
		f.room -= k
		f.left -= k
	}
}

// AppendSidebandPack streams the pack of objects into dst as side-band-64k band-1 packets: the same bytes as
// side-banding WritePack's output, without ever materializing the bare pack.
func AppendSidebandPack(dst []byte, objects []*GitObject) []byte {
	streams, size := preparePack(objects)
	chunks := (size + SidebandChunk - 1) / SidebandChunk
	dst = slices.Grow(dst, size+5*chunks)
	f := &sidebandFramer{out: dst, left: size, band: 1}
	emitPack(objects, streams, f.write)
	return f.out
}

// ---------------------------------------------------------------- caches

// cachedFrame returns the reply cached under key for the current RefsVersion, building it on a miss. The caller
// holds g.Mu (read or write), so RefsVersion is stable; any ref change empties the cache on the next access.
func (g *GitRepo) cachedFrame(key string, build func() []byte) []byte {
	ver := atomic.LoadUint64(&g.RefsVersion)
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	if g.cache == nil || g.cacheVersion != ver {
		g.cache = make(map[string][]byte)
		g.cacheVersion = ver
	}
	if frame, ok := g.cache[key]; ok {
		return frame
	}
	frame := build()
	g.cache[key] = frame
	return frame
}

func (g *GitRepo) fastAdvertV0(service string) []byte {
	upload := service == "git-upload-pack"
	ver := atomic.LoadUint64(&g.RefsVersion)
	if rf := g.refFrames.Load(); rf != nil && rf.version == ver {
		if upload && rf.uploadAdvert != nil {
			return rf.uploadAdvert
		}
		if !upload && rf.receiveAdvert != nil {
			return rf.receiveAdvert
		}
	}
	g.Mu.RLock()
	defer g.Mu.RUnlock()
	ver = atomic.LoadUint64(&g.RefsVersion)
	var next gitRefFrames
	if rf := g.refFrames.Load(); rf != nil && rf.version == ver {
		if upload && rf.uploadAdvert != nil {
			return rf.uploadAdvert
		}
		if !upload && rf.receiveAdvert != nil {
			return rf.receiveAdvert
		}
		next = *rf
	} else {
		next.version = ver
	}
	frame := advertisementV0(g, service)
	if upload {
		next.uploadAdvert = frame
	} else {
		next.receiveAdvert = frame
	}
	g.refFrames.Store(&next)
	return frame
}

func (g *GitRepo) fastLsRefsPlain(mask int) []byte {
	ver := atomic.LoadUint64(&g.RefsVersion)
	if rf := g.refFrames.Load(); rf != nil && rf.version == ver && rf.lsRefsPlain[mask] != nil {
		return rf.lsRefsPlain[mask]
	}
	peel := mask&4 != 0
	symrefs := mask&2 != 0
	unborn := mask&1 != 0
	g.Mu.RLock()
	defer g.Mu.RUnlock()
	ver = atomic.LoadUint64(&g.RefsVersion)
	var next gitRefFrames
	if rf := g.refFrames.Load(); rf != nil && rf.version == ver {
		if rf.lsRefsPlain[mask] != nil {
			return rf.lsRefsPlain[mask]
		}
		next = *rf
	} else {
		next.version = ver
	}
	refs := g.RefsWithPrefix("refs/")
	buf := newFrameBody(128 + 112*len(refs))
	headTarget := g.HeadRefName()
	headOID, hasHead := g.refs[headTarget]
	switch {
	case hasHead && symrefs:
		buf = appendPkt(buf, headOID, " HEAD symref-target:", headTarget, "\n")
	case hasHead:
		buf = appendPkt(buf, headOID, " HEAD\n")
	case unborn:
		buf = appendPkt(buf, "unborn HEAD symref-target:", headTarget, "\n")
	}
	for _, ref := range refs {
		peeled := ""
		if peel {
			peeled = g.peeledOf(ref.OID)
		}
		if peeled != "" {
			buf = appendPkt(buf, ref.OID, " ", ref.Name, " peeled:", peeled, "\n")
		} else {
			buf = appendPkt(buf, ref.OID, " ", ref.Name, "\n")
		}
	}
	buf = append(buf, pktFlushLine...)
	frame := finishFrame(buf, 200, ctUploadResult, GitNoCacheHeaders)
	next.lsRefsPlain[mask] = frame
	g.refFrames.Store(&next)
	return frame
}

func isOIDBytes(b []byte) bool {
	if len(b) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := b[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// tryFastUploadV2 handles standard v2 ls-refs (no ref-prefix) and single-want fresh clone fetches without
// packet-slice/string allocations or mutex contention across 50 concurrent clones. Returns nil to fall back.
func tryFastUploadV2(g *GitRepo, body []byte) []byte {
	const (
		cmdNone   = 0
		cmdLsRefs = 1
		cmdFetch  = 2
	)
	cmd := cmdNone
	inArgs := false
	flushed := false
	mask := 0
	var want [40]byte
	wantCount := 0
	depth := int64(0)
	done := false

	pos := 0
	for pos < len(body) {
		if pos+4 > len(body) {
			return nil
		}
		length := 0
		for k := 0; k < 4; k++ {
			v, ok := hexNibble(body[pos+k])
			if !ok {
				return nil
			}
			length = length<<4 | int(v)
		}
		switch length {
		case 0:
			pos += 4
			if pos != len(body) {
				return nil // trailing packets after flush: let full parser validate
			}
			flushed = true
		case 1:
			pos += 4
			if inArgs {
				return nil
			}
			inArgs = true
			continue
		case 2:
			pos += 4
			continue
		default:
			if length < 4 || pos+length > len(body) {
				return nil
			}
			line := body[pos+4 : pos+length]
			pos += length
			if len(line) > 0 && line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			if !inArgs {
				if cmd == cmdNone && bytes.HasPrefix(line, []byte("command=")) {
					switch string(line) {
					case "command=ls-refs":
						cmd = cmdLsRefs
					case "command=fetch":
						cmd = cmdFetch
					default:
						return nil
					}
				}
				continue
			}
			if cmd == cmdLsRefs {
				switch string(line) {
				case "peel":
					mask |= 4
				case "symrefs":
					mask |= 2
				case "unborn":
					mask |= 1
				default:
					return nil
				}
				continue
			}
			if cmd == cmdFetch {
				switch {
				case string(line) == "thin-pack" || string(line) == "no-progress" || string(line) == "ofs-delta" || string(line) == "include-tag":
				case string(line) == "done":
					done = true
				case bytes.HasPrefix(line, []byte("want ")):
					oid := line[5:]
					if !isOIDBytes(oid) || wantCount > 0 {
						return nil
					}
					copy(want[:], oid)
					wantCount = 1
				case bytes.HasPrefix(line, []byte("deepen ")):
					raw := line[7:]
					if len(raw) == 0 || len(raw) > 9 {
						return nil
					}
					var d int64
					for _, ch := range raw {
						if ch < '0' || ch > '9' {
							return nil
						}
						d = d*10 + int64(ch-'0')
					}
					if d < 1 {
						d = 1
					}
					depth = d
				default:
					return nil
				}
			}
		}
	}
	if !flushed {
		return nil
	}
	if cmd == cmdLsRefs {
		return g.fastLsRefsPlain(mask)
	}
	if cmd == cmdFetch && wantCount == 1 && done {
		if fc := g.fastClone.Load(); fc != nil && fc.v2 && fc.depth == depth && fc.want == want {
			return fc.frame
		}
		wantStr := string(want[:])
		a := fetchArgs{wants: []string{wantStr}, depth: depth, done: true}
		var builtOK bool
		frame := cloneFrame(cloneKey("v2:", a.wants, a.depth), func() ([]byte, int, bool) {
			f, n, ok := buildFetchV2(g, &a, nil)
			builtOK = ok
			return f, n, ok
		})
		if builtOK || (len(frame) > 0 && !bytes.Contains(frame, []byte("ERR "))) {
			g.fastClone.Store(&fastCloneItem{want: want, depth: depth, v2: true, frame: frame})
		}
		return frame
	}
	return nil
}

const (
	cloneCacheLimit    = 256      // entries, oldest evicted first
	cloneEntryMaxBytes = 16 << 20 // bodies above this are served but not kept
	cloneCacheMaxBytes = 64 << 20 // total budget (the sandbox has 1 GiB in all)
)

type cloneFlight struct {
	done  chan struct{}
	frame []byte
	ok    bool // a pack reply (not an ERR), valid for every waiter
}

// clones holds whole fetch replies for fresh clones, keyed by what they contain. Content-addressed, so a reply
// is valid for any repository holding those wants.
var clones = struct {
	sync.Mutex
	entries map[string][]byte
	order   []string
	bytes   int
	flights map[string]*cloneFlight
}{entries: map[string][]byte{}, flights: map[string]*cloneFlight{}}

// cloneFrame returns the cached reply for key, or builds it once however many requests ask concurrently: the
// first becomes the leader, the rest wait for its frame. A leader whose answer was an ERR (repository-specific)
// does not speak for the waiters; each then builds its own.
func cloneFrame(key string, build func() (frame []byte, bodyLen int, ok bool)) []byte {
	clones.Lock()
	if frame, hit := clones.entries[key]; hit {
		clones.Unlock()
		return frame
	}
	if flight := clones.flights[key]; flight != nil {
		clones.Unlock()
		<-flight.done
		if flight.ok {
			return flight.frame
		}
		frame, _, _ := build()
		return frame
	}
	flight := &cloneFlight{done: make(chan struct{})}
	clones.flights[key] = flight
	clones.Unlock()

	finished := false
	defer func() {
		if !finished { // the build panicked: release the waiters, who then build for themselves
			clones.Lock()
			delete(clones.flights, key)
			clones.Unlock()
			close(flight.done)
		}
	}()
	frame, bodyLen, ok := build()
	flight.frame, flight.ok = frame, ok
	clones.Lock()
	delete(clones.flights, key)
	if ok {
		rememberClone(key, frame, bodyLen)
	}
	clones.Unlock()
	finished = true
	close(flight.done)
	return frame
}

// rememberClone stores a reply (clones is locked), evicting the oldest entries to stay within the limits.
func rememberClone(key string, frame []byte, bodyLen int) {
	if bodyLen > cloneEntryMaxBytes {
		return
	}
	if _, exists := clones.entries[key]; exists {
		return
	}
	for len(clones.order) > 0 && (len(clones.entries) >= cloneCacheLimit || clones.bytes+len(frame) > cloneCacheMaxBytes) {
		oldest := clones.order[0]
		clones.order = clones.order[1:]
		clones.bytes -= len(clones.entries[oldest])
		delete(clones.entries, oldest)
	}
	clones.entries[key] = frame
	clones.order = append(clones.order, key)
	clones.bytes += len(frame)
}

func cloneKey(prefix string, wants []string, depth int64) string {
	unique := slices.Clone(wants)
	slices.Sort(unique)
	unique = slices.Compact(unique)
	return prefix + strings.Join(unique, ",") + ":" + strconv.FormatInt(depth, 10)
}

// ---------------------------------------------------------------- advertisements

// peeledOf is what an annotated tag finally points at (at most 16 hops), or "" for a non-tag or a broken chain.
func (g *GitRepo) peeledOf(oid string) string {
	current := g.objects[oid]
	if current == nil || current.Type != ObjTag {
		return ""
	}
	for i := 0; i < 16 && current != nil && current.Type == ObjTag; i++ {
		current = g.objects[ParseTag(current.Data).Object]
		if current != nil && current.Type != ObjTag {
			return current.OID
		}
	}
	return ""
}

func advertisementV2() []byte {
	var b []byte
	b = appendPkt(b, "version 2\n")
	b = appendPkt(b, "agent=", gitAgent, "\n")
	b = appendPkt(b, "ls-refs=unborn\n")
	b = appendPkt(b, "fetch=shallow\n")
	b = appendPkt(b, "server-option\n")
	b = appendPkt(b, "object-format=sha1\n")
	return append(b, pktFlushLine...)
}

// advertisementV0 renders the v0 /info/refs reply: the service line, then HEAD (upload-pack only, when the
// default branch exists) and every ref with the capabilities on the first line, peeled tags as "^{}" lines for
// upload-pack, or the "capabilities^{}" line for an empty repository.
func advertisementV0(g *GitRepo, service string) []byte {
	upload := service == "git-upload-pack"
	refs := g.RefsWithPrefix("refs/")
	buf := newFrameBody(256 + len(uploadCaps) + 96*len(refs))
	buf = appendPkt(buf, "# service=", service, "\n")
	buf = append(buf, pktFlushLine...)
	headTarget := g.HeadRefName()
	headOID, hasHead := g.refs[headTarget]
	caps := receiveCaps
	if upload {
		caps = uploadCaps
		if hasHead {
			caps = "symref=HEAD:" + headTarget + " " + caps
		}
	}
	caps += " agent=" + gitAgent
	first := true
	line := func(oid, name string) {
		if first {
			buf = appendPkt(buf, oid, " ", name, "\x00", caps, "\n")
			first = false
		} else {
			buf = appendPkt(buf, oid, " ", name, "\n")
		}
		if upload && name != "HEAD" {
			if peeled := g.peeledOf(oid); peeled != "" {
				buf = appendPkt(buf, peeled, " ", name, "^{}\n")
			}
		}
	}
	if upload && hasHead {
		line(headOID, "HEAD")
	}
	for _, ref := range refs {
		line(ref.OID, ref.Name)
	}
	if first {
		buf = appendPkt(buf, ZeroOID, " capabilities^{}\x00", caps, "\n")
	}
	buf = append(buf, pktFlushLine...)
	contentType := ctReceiveAdvert
	if upload {
		contentType = ctUploadAdvert
	}
	return finishFrame(buf, 200, contentType, GitNoCacheHeaders)
}

// ---------------------------------------------------------------- ls-refs

func lsRefsFrame(g *GitRepo, args []string) []byte {
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, arg[11:])
		}
	}
	key := make([]byte, 0, 16+32*len(prefixes))
	key = append(key, "ls:"...)
	for _, flag := range [3]bool{peel, symrefs, unborn} {
		if flag {
			key = append(key, '1')
		} else {
			key = append(key, '0')
		}
	}
	for _, prefix := range prefixes { // length-prefixed: no two prefix lists share a key
		key = append(key, ':')
		key = strconv.AppendInt(key, int64(len(prefix)), 10)
		key = append(key, ':')
		key = append(key, prefix...)
	}
	g.Mu.RLock()
	defer g.Mu.RUnlock()
	return g.cachedFrame(string(key), func() []byte {
		matches := func(name string) bool {
			if len(prefixes) == 0 {
				return true
			}
			for _, prefix := range prefixes {
				if strings.HasPrefix(name, prefix) {
					return true
				}
			}
			return false
		}
		refs := g.RefsWithPrefix("refs/")
		buf := newFrameBody(128 + 112*len(refs))
		headTarget := g.HeadRefName()
		headOID, hasHead := g.refs[headTarget]
		if matches("HEAD") {
			switch {
			case hasHead && symrefs:
				buf = appendPkt(buf, headOID, " HEAD symref-target:", headTarget, "\n")
			case hasHead:
				buf = appendPkt(buf, headOID, " HEAD\n")
			case unborn:
				buf = appendPkt(buf, "unborn HEAD symref-target:", headTarget, "\n")
			}
		}
		for _, ref := range refs {
			if !matches(ref.Name) {
				continue
			}
			peeled := ""
			if peel {
				peeled = g.peeledOf(ref.OID)
			}
			if peeled != "" {
				buf = appendPkt(buf, ref.OID, " ", ref.Name, " peeled:", peeled, "\n")
			} else {
				buf = appendPkt(buf, ref.OID, " ", ref.Name, "\n")
			}
		}
		buf = append(buf, pktFlushLine...)
		return finishFrame(buf, 200, ctUploadResult, GitNoCacheHeaders)
	})
}

// ---------------------------------------------------------------- fetch planning

type fetchArgs struct {
	wants    []string
	haves    []string
	shallow  []string
	wantRefs []string
	depth    int64   // 0 when not deepening, else >= 1
	since    float64 // deepen-since; NaN when unparsable (parseInt semantics), meaningful only with hasSince
	hasSince bool
	done     bool
}

type fetchPlan struct {
	objects   []*GitObject
	shallow   []string
	unshallow []string
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrim is String.prototype.trim.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSSpace)
}

// jsParseInt is parseInt(s, 10): leading whitespace, an optional sign, then the longest digit run; NaN without digits.
func jsParseInt(s string) float64 {
	s = strings.TrimLeftFunc(s, isJSSpace)
	negative := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		negative = s[0] == '-'
		s = s[1:]
	}
	end := 0
	for end < len(s) && isDigit(s[end]) {
		end++
	}
	if end == 0 {
		return math.NaN()
	}
	v, _ := strconv.ParseFloat(s[:end], 64) // digits only: always parses (to +Inf when huge)
	if negative {
		v = -v
	}
	return v
}

// parseDepth is Math.max(1, parseInt(s, 10) || 1).
func parseDepth(s string) int64 {
	v := jsParseInt(s)
	if math.IsNaN(v) || v < 1 {
		return 1
	}
	if v > 1<<53 {
		return 1 << 53
	}
	return int64(v)
}

func parseFetchArgs(args []string) fetchArgs {
	var out fetchArgs
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			out.wants = append(out.wants, jsTrim(arg[5:]))
		case strings.HasPrefix(arg, "want-ref "):
			out.wantRefs = append(out.wantRefs, jsTrim(arg[9:]))
		case strings.HasPrefix(arg, "have "):
			out.haves = append(out.haves, jsTrim(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			out.shallow = append(out.shallow, jsTrim(arg[8:]))
		case strings.HasPrefix(arg, "deepen "):
			out.depth = parseDepth(arg[7:])
		case strings.HasPrefix(arg, "deepen-since "):
			out.since, out.hasSince = jsParseInt(arg[13:]), true
		case arg == "done":
			out.done = true
		}
	}
	return out
}

// objectCollector gathers the objects a fetch sends: depth-first, pre-order, skipping submodule links, objects
// already sent and objects the client provably has.
type objectCollector struct {
	g        *GitRepo
	seen     map[string]struct{}
	excluded map[string]struct{}
	objects  []*GitObject
}

func (c *objectCollector) add(oid string) {
	if _, ok := c.seen[oid]; ok {
		return
	}
	if _, ok := c.excluded[oid]; ok {
		return
	}
	obj := c.g.objects[oid]
	if obj == nil {
		return
	}
	c.seen[oid] = struct{}{}
	c.objects = append(c.objects, obj)
	if obj.Type != ObjTree {
		return
	}
	for _, entry := range obj.Tree() {
		if entry.Mode != "160000" {
			c.add(entry.OID)
		}
	}
}

// markTree marks a tree and everything under it as held by the client.
func (g *GitRepo) markTree(oid string, excluded map[string]struct{}) {
	if _, ok := excluded[oid]; ok {
		return
	}
	obj := g.objects[oid]
	if obj == nil {
		return
	}
	excluded[oid] = struct{}{}
	if obj.Type != ObjTree {
		return
	}
	for _, entry := range obj.Tree() {
		switch entry.Mode {
		case "160000":
		case "40000":
			g.markTree(entry.OID, excluded)
		default:
			excluded[entry.OID] = struct{}{}
		}
	}
}

// planFetch decides what a fetch sends: the wants (tags peeled, at most 16 hops) minus everything the client
// provably has, cut off at the requested depth or date. With collect false only the shallow lists are computed.
// The caller holds g.Mu for reading.
func planFetch(g *GitRepo, a *fetchArgs, collect bool) (fetchPlan, error) {
	clientShallow := make(map[string]struct{}, len(a.shallow))
	for _, oid := range a.shallow {
		clientShallow[oid] = struct{}{}
	}
	var tags []*GitObject
	var wantCommits, loose []string
	tagSeen := map[*GitObject]struct{}{}
	wantSeen := map[string]struct{}{}
	for _, want := range a.wants {
		oid := want
		for hop := 0; hop < 16; hop++ {
			obj := g.objects[oid]
			if obj == nil {
				return fetchPlan{}, &GitProtocolError{Msg: "upload-pack: not our ref " + want}
			}
			if obj.Type == ObjTag {
				if _, ok := tagSeen[obj]; !ok {
					tagSeen[obj] = struct{}{}
					tags = append(tags, obj)
				}
				oid = ParseTag(obj.Data).Object
				continue
			}
			if obj.Type == ObjCommit {
				if _, ok := wantSeen[oid]; !ok {
					wantSeen[oid] = struct{}{}
					wantCommits = append(wantCommits, oid)
				}
			} else {
				loose = append(loose, oid)
			}
			break
		}
	}
	var haveCommits []string
	haveSeen := map[string]struct{}{}
	for _, have := range a.haves {
		if !g.HasObject(have) {
			continue
		}
		if commit := g.PeelToCommit(have); commit != "" {
			if _, ok := haveSeen[commit]; !ok {
				haveSeen[commit] = struct{}{}
				haveCommits = append(haveCommits, commit)
			}
		}
	}
	// Commits the client has: its haves and their history, stopping at its own shallow boundary.
	known := make(map[string]struct{}, len(haveCommits))
	walk := slices.Clone(haveCommits)
	for _, oid := range haveCommits {
		known[oid] = struct{}{}
	}
	for head := 0; head < len(walk); head++ {
		oid := walk[head]
		if _, ok := clientShallow[oid]; ok {
			continue
		}
		info := g.Commit(oid)
		if info == nil {
			continue
		}
		for _, parent := range info.Parents {
			if _, ok := known[parent]; !ok {
				known[parent] = struct{}{}
				walk = append(walk, parent)
			}
		}
	}
	level := make(map[string]int64, len(wantCommits))
	queue := make([]string, 0, len(wantCommits))
	for _, want := range wantCommits {
		level[want] = 1
		queue = append(queue, want)
	}
	var sendCommits, edges []string
	plan := fetchPlan{shallow: []string{}, unshallow: []string{}}
	deepening := a.depth > 0
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		at := level[oid]
		info := g.Commit(oid)
		if info == nil {
			continue
		}
		_, isClientShallow := clientShallow[oid]
		reopens := deepening && isClientShallow && at < a.depth
		if reopens {
			plan.unshallow = append(plan.unshallow, oid)
		}
		var traverse bool
		if _, ok := known[oid]; ok {
			edges = append(edges, oid)
			traverse = reopens
		} else {
			sendCommits = append(sendCommits, oid)
			traverse = true
			if deepening && at >= a.depth {
				traverse = false
				if len(info.Parents) > 0 {
					plan.shallow = append(plan.shallow, oid)
				}
			} else if a.hasSince && len(info.Parents) > 0 {
				older := 0
				for _, parent := range info.Parents {
					var when int64
					if parentInfo := g.Commit(parent); parentInfo != nil {
						when = parentInfo.Committer.When
					}
					if float64(when) < a.since {
						older++
					}
				}
				if older == len(info.Parents) {
					traverse = false
					plan.shallow = append(plan.shallow, oid)
				}
			}
		}
		if traverse {
			for _, parent := range info.Parents {
				if _, ok := level[parent]; !ok {
					level[parent] = at + 1
					queue = append(queue, parent)
				}
			}
		}
	}
	if !collect {
		return plan, nil
	}
	// Objects the client already holds: the trees of its have and edge commits.
	excluded := map[string]struct{}{}
	for _, list := range [2][]string{haveCommits, edges} {
		for _, oid := range list {
			if info := g.Commit(oid); info != nil {
				g.markTree(info.Tree, excluded)
			}
		}
	}
	c := objectCollector{
		g:        g,
		seen:     make(map[string]struct{}, len(tags)+4*len(sendCommits)),
		excluded: excluded,
		objects:  make([]*GitObject, 0, len(tags)+4*len(sendCommits)),
	}
	for _, tag := range tags {
		c.objects = append(c.objects, tag)
		c.seen[tag.OID] = struct{}{}
	}
	for _, oid := range sendCommits {
		c.seen[oid] = struct{}{}
		c.objects = append(c.objects, g.objects[oid])
	}
	for _, oid := range sendCommits {
		c.add(g.objects[oid].Commit().Tree)
	}
	for _, oid := range loose {
		c.add(oid)
	}
	plan.objects = c.objects
	return plan, nil
}

// ---------------------------------------------------------------- upload-pack v2

func errFrame(message string) []byte {
	return uploadResultFrame(appendPkt(nil, "ERR ", message, "\n"))
}

func fetchV2Frame(g *GitRepo, args []string) []byte {
	a := parseFetchArgs(args)
	var wanted []Ref
	if len(a.wantRefs) > 0 {
		missing := ""
		func() {
			g.Mu.RLock()
			defer g.Mu.RUnlock()
			for _, ref := range a.wantRefs {
				oid, ok := g.refs[ref]
				if !ok {
					missing = ref
					return
				}
				wanted = append(wanted, Ref{Name: ref, OID: oid})
				a.wants = append(a.wants, oid)
			}
		}()
		if missing != "" {
			return errFrame("upload-pack: unknown ref " + missing)
		}
	}
	for _, want := range a.wants {
		if !IsOID(want) {
			return errFrame("upload-pack: protocol error: expected want <oid>")
		}
	}
	if len(a.haves) == 0 && len(a.shallow) == 0 && !a.hasSince && a.done && len(wanted) == 0 {
		return cloneFrame(cloneKey("v2:", a.wants, a.depth), func() ([]byte, int, bool) { return buildFetchV2(g, &a, nil) })
	}
	frame, _, _ := buildFetchV2(g, &a, wanted)
	return frame
}

// buildFetchV2 renders a v2 fetch reply: acknowledgments (unless done), shallow-info, wanted-refs, packfile.
// ok is true for a pack reply.
func buildFetchV2(g *GitRepo, a *fetchArgs, wanted []Ref) (frame []byte, bodyLen int, ok bool) {
	buf := newFrameBody(512)
	var plan fetchPlan
	var planErr error
	negotiated := func() bool {
		g.Mu.RLock()
		defer g.Mu.RUnlock()
		if !a.done {
			var common []string
			for _, have := range a.haves {
				if g.HasObject(have) {
					common = append(common, have)
				}
			}
			buf = appendPkt(buf, "acknowledgments\n")
			if len(common) == 0 {
				buf = appendPkt(buf, "NAK\n")
				buf = append(buf, pktFlushLine...)
				return false
			}
			for _, oid := range common {
				buf = appendPkt(buf, "ACK ", oid, "\n")
			}
			buf = appendPkt(buf, "ready\n")
			buf = append(buf, pktDelimLine...)
		}
		plan, planErr = planFetch(g, a, true)
		return true
	}()
	if !negotiated {
		return finishFrame(buf, 200, ctUploadResult, GitNoCacheHeaders), 0, false
	}
	if planErr != nil {
		return errFrame(planErr.Error()), 0, false
	}
	// From here on no lock: the plan holds immutable objects, and deflate + SHA-1 run unlocked.
	if a.depth > 0 || a.hasSince {
		buf = appendPkt(buf, "shallow-info\n")
		for _, oid := range plan.shallow {
			buf = appendPkt(buf, "shallow ", oid, "\n")
		}
		for _, oid := range plan.unshallow {
			buf = appendPkt(buf, "unshallow ", oid, "\n")
		}
		buf = append(buf, pktDelimLine...)
	}
	if len(wanted) > 0 {
		buf = appendPkt(buf, "wanted-refs\n")
		for _, ref := range wanted {
			buf = appendPkt(buf, ref.OID, " ", ref.Name, "\n")
		}
		buf = append(buf, pktDelimLine...)
	}
	buf = appendPkt(buf, "packfile\n")
	buf = AppendSidebandPack(buf, plan.objects)
	buf = append(buf, pktFlushLine...)
	bodyLen = len(buf) - frameGap
	return finishFrame(buf, 200, ctUploadResult, GitNoCacheHeaders), bodyLen, true
}

// ---------------------------------------------------------------- upload-pack v0

func fetchV0Frame(g *GitRepo, packets []gitPacket) []byte {
	var a fetchArgs
	for _, packet := range packets {
		if packet.kind == pktFlush {
			continue
		}
		line := packetText(packet)
		switch {
		case strings.HasPrefix(line, "want "):
			a.wants = append(a.wants, line[5:min(45, len(line))])
		case strings.HasPrefix(line, "have "):
			a.haves = append(a.haves, line[5:min(45, len(line))])
		case strings.HasPrefix(line, "shallow "):
			a.shallow = append(a.shallow, jsTrim(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			a.depth = parseDepth(line[7:])
		case strings.HasPrefix(line, "deepen-since "):
			a.since, a.hasSince = jsParseInt(line[13:]), true
		case line == "done":
			a.done = true
		}
	}
	if len(a.wants) == 0 {
		return uploadResultFrame([]byte(pktFlushLine))
	}
	for _, want := range a.wants {
		if !IsOID(want) {
			return errFrame("upload-pack: protocol error: expected want <oid>")
		}
	}
	// A deepening stateless (HTTP) v0 client first sends nothing but its wants section and reads back only
	// the shallow list; git's upload-pack stops right there when the request ends after that flush. (git.ts
	// answered NAK as well, which made the client's next round die with "expected shallow list".)
	if (a.depth > 0 || a.hasSince) && endsAfterFirstFlush(packets) {
		frame, _, _ := buildFetchV0(g, &a, true)
		return frame
	}
	if len(a.haves) == 0 && len(a.shallow) == 0 && !a.hasSince && a.done {
		return cloneFrame(cloneKey("v0:", a.wants, a.depth), func() ([]byte, int, bool) { return buildFetchV0(g, &a, false) })
	}
	frame, _, _ := buildFetchV0(g, &a, false)
	return frame
}

// endsAfterFirstFlush reports whether the request is only a wants section: one flush, and it is the last packet.
func endsAfterFirstFlush(packets []gitPacket) bool {
	for i, packet := range packets {
		if packet.kind == pktFlush {
			return i == len(packets)-1
		}
	}
	return false
}

// buildFetchV0 renders a v0 (stateless) reply: shallow lines and a flush when deepening, then NAK or the first
// common have's ACK, then — once the client said done — the side-banded pack and a flush. With shallowOnly the
// reply stops after the shallow list.
func buildFetchV0(g *GitRepo, a *fetchArgs, shallowOnly bool) (frame []byte, bodyLen int, ok bool) {
	var plan fetchPlan
	var err error
	common := ""
	func() {
		g.Mu.RLock()
		defer g.Mu.RUnlock()
		if plan, err = planFetch(g, a, a.done && !shallowOnly); err != nil {
			return
		}
		for _, have := range a.haves {
			if g.HasObject(have) {
				common = have
				break
			}
		}
	}()
	if err != nil {
		return errFrame(err.Error()), 0, false
	}
	buf := newFrameBody(256)
	if a.depth > 0 || a.hasSince {
		for _, oid := range plan.shallow {
			buf = appendPkt(buf, "shallow ", oid, "\n")
		}
		for _, oid := range plan.unshallow {
			buf = appendPkt(buf, "unshallow ", oid, "\n")
		}
		buf = append(buf, pktFlushLine...)
	}
	if shallowOnly {
		return finishFrame(buf, 200, ctUploadResult, GitNoCacheHeaders), 0, false
	}
	if common == "" {
		buf = appendPkt(buf, "NAK\n")
	} else {
		buf = appendPkt(buf, "ACK ", common, "\n")
	}
	if a.done {
		buf = AppendSidebandPack(buf, plan.objects)
		buf = append(buf, pktFlushLine...)
	}
	bodyLen = len(buf) - frameGap
	return finishFrame(buf, 200, ctUploadResult, GitNoCacheHeaders), bodyLen, a.done
}

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// parsePushCommand matches /^([0-9a-f]{40}) ([0-9a-f]{40}) (.+)$/ ("." excluding line terminators).
func parsePushCommand(line string) (pushCommand, bool) {
	if len(line) < 83 || line[40] != ' ' || line[81] != ' ' {
		return pushCommand{}, false
	}
	cmd := pushCommand{oldOID: line[:40], newOID: line[41:81], ref: line[82:]}
	if !IsOID(cmd.oldOID) || !IsOID(cmd.newOID) || strings.ContainsAny(cmd.ref, "\r\n  ") {
		return pushCommand{}, false
	}
	return cmd, true
}

// missingLink checks that everything the new objects point at exists; it describes the first gap, or "".
// (A malformed tree is reported here instead of failing the whole request.)
func (g *GitRepo) missingLink(objects []*GitObject) string {
	for _, obj := range objects {
		switch obj.Type {
		case ObjCommit:
			info := obj.Commit()
			if !g.HasObject(info.Tree) {
				return "missing tree " + info.Tree
			}
			for _, parent := range info.Parents {
				if !g.HasObject(parent) {
					return "missing parent " + parent
				}
			}
		case ObjTree:
			entries, err := obj.TreeChecked()
			if err != nil {
				return err.Error()
			}
			for _, entry := range entries {
				if entry.Mode != "160000" && !g.HasObject(entry.OID) {
					return "missing " + entry.OID
				}
			}
		case ObjTag:
			if !g.HasObject(ParseTag(obj.Data).Object) {
				return "missing tag target"
			}
		}
	}
	return ""
}

func (g *GitRepo) pushFastForward(from, to string) bool {
	if info := g.Commit(to); info != nil && slices.Contains(info.Parents, from) {
		return true
	}
	return g.IsAncestor(from, to)
}

// pushVerdict is "" when the command may be applied, else the reason it is refused. g.Mu is write-locked.
func (g *GitRepo) pushVerdict(cmd pushCommand, unpack, linkProblem string) string {
	if unpack != "ok" {
		return "unpacker error"
	}
	if !IsRefNameValid(cmd.ref) {
		return "funny refname"
	}
	current, ok := g.refs[cmd.ref]
	if !ok {
		current = ZeroOID
	}
	if cmd.newOID == ZeroOID {
		if current == ZeroOID {
			return "failed to delete"
		}
		if cmd.ref == g.HeadRefName() {
			return "deletion of the current branch prohibited"
		}
		if cmd.oldOID != current && cmd.oldOID != ZeroOID {
			return "failed to update ref"
		}
		return ""
	}
	if cmd.oldOID != current {
		return "failed to update ref"
	}
	target := g.objects[cmd.newOID]
	if target == nil || linkProblem != "" {
		return "missing necessary objects"
	}
	if strings.HasPrefix(cmd.ref, "refs/heads/") && target.Type != ObjCommit {
		return "not a commit"
	}
	if current != ZeroOID {
		existing := g.objects[current]
		if existing == nil || existing.Type != ObjCommit || target.Type != ObjCommit || !g.pushFastForward(current, cmd.newOID) {
			return "non-fast-forward"
		}
	}
	return ""
}

// receivePack runs one push: commands, pack, verdicts, ref updates and the report. Only a malformed command
// section is an error (400); everything else is reported in-band.
func receivePack(g *GitRepo, body []byte) ([]byte, error) {
	packets, end, err := parsePackets(body, 0, true)
	if err != nil {
		return nil, err
	}
	var commands []pushCommand
	caps := map[string]bool{}
	for _, packet := range packets {
		if packet.kind != pktData {
			continue
		}
		line := strings.TrimSuffix(string(packet.data), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			caps = map[string]bool{}
			for _, capability := range strings.Split(line[nul+1:], " ") {
				if capability != "" {
					caps[capability] = true
				}
			}
			line = line[:nul]
		}
		if strings.HasPrefix(line, "shallow ") || strings.HasPrefix(line, "push-cert") {
			continue
		}
		cmd, ok := parsePushCommand(line)
		if !ok {
			return nil, &GitProtocolError{Msg: "bad command " + jsQuote(utf16Prefix(line, 80))}
		}
		commands = append(commands, cmd)
	}
	wantsReport := caps["report-status"] || caps["report-status-v2"]
	useSideband := caps["side-band-64k"]

	unpack := "ok"
	var added []*GitObject
	if packData := body[end:]; len(packData) > 0 && slices.ContainsFunc(commands, func(c pushCommand) bool { return c.newOID != ZeroOID }) {
		// Inflate, resolve deltas and hash with no write lock held, so fetches keep being served; thin-pack
		// bases come from the store under short read locks. Objects are only ever added, so a base seen here
		// is still there below. Nothing is deflated: outgoing packs compress lazily.
		objects, err := ParsePack(packData, func(oid string) *GitObject {
			g.Mu.RLock()
			defer g.Mu.RUnlock()
			return g.objects[oid]
		})
		if err != nil {
			unpack = err.Error()
		} else {
			added = objects
		}
	}

	// Lock order is storeMu, then the repository: REST/GraphQL/MCP read git state under storeMu alone.
	storeMu.Lock()
	defer storeMu.Unlock()
	g.Mu.Lock()
	defer g.Mu.Unlock()
	for _, obj := range added {
		g.AddObject(obj)
	}
	linkProblem := ""
	if unpack == "ok" {
		linkProblem = g.missingLink(added)
	}
	verdicts := make([]string, len(commands))
	refused := false
	for i, cmd := range commands {
		verdicts[i] = g.pushVerdict(cmd, unpack, linkProblem)
		refused = refused || verdicts[i] != ""
	}
	if caps["atomic"] && refused {
		for i := range verdicts {
			if verdicts[i] == "" {
				verdicts[i] = "atomic push failed"
			}
		}
	}
	hadBranches := g.hasRefWithPrefix("refs/heads/")
	for i, cmd := range commands {
		if verdicts[i] != "" {
			continue
		}
		if cmd.newOID == ZeroOID {
			g.DeleteRef(cmd.ref)
			continue
		}
		g.SetRef(cmd.ref, cmd.newOID)
		// The first branch pushed into a repository without branches becomes the default branch when the
		// configured default does not exist.
		if !hadBranches && strings.HasPrefix(cmd.ref, "refs/heads/") && !g.HasRef(g.HeadRefName()) {
			g.DefaultBranch = cmd.ref[len("refs/heads/"):]
		}
	}
	if !wantsReport {
		if useSideband {
			return []byte(pktFlushLine), nil
		}
		return []byte{}, nil
	}
	report := appendPkt(nil, "unpack ", unpack, "\n")
	for i, cmd := range commands {
		if verdicts[i] == "" {
			report = appendPkt(report, "ok ", cmd.ref, "\n")
		} else {
			report = appendPkt(report, "ng ", cmd.ref, " ", verdicts[i], "\n")
		}
	}
	report = append(report, pktFlushLine...)
	if !useSideband {
		return report, nil
	}
	out := appendSideband(make([]byte, 0, len(report)+16), report, 1)
	return append(out, pktFlushLine...), nil
}

// ---------------------------------------------------------------- request helpers

var errGitBodyTooLarge = errors.New("request body too large")

// decodeGitBody undoes Content-Encoding gzip / x-gzip (exact, case-sensitive values); other bodies pass through.
func decodeGitBody(body []byte, encoding string) ([]byte, error) {
	if encoding != "gzip" && encoding != "x-gzip" {
		return body, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(zr, maxGitBody+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxGitBody {
		return nil, errGitBodyTooLarge
	}
	return out, nil
}

var okMainSidebandFrame = func() []byte {
	var report []byte
	report = appendPkt(report, "unpack ok\n")
	report = appendPkt(report, "ok refs/heads/main\n")
	report = append(report, pktFlushLine...)
	out := appendSideband(make([]byte, 0, len(report)+16), report, 1)
	out = append(out, pktFlushLine...)
	return HTTPFrame(200, ctReceiveResult, GitNoCacheHeaders, out)
}()

// tryFastReceivePack handles the common single-command ref update push with zero command/map/report allocations.
// Unusual command lists (multiple commands, deletes, shallow/push-cert lines, malformed headers) return nil so
// receivePack handles them with full generality.
func tryFastReceivePack(g *GitRepo, body []byte) []byte {
	if len(body) < 91 {
		return nil
	}
	length := 0
	for k := 0; k < 4; k++ {
		v, ok := hexNibble(body[k])
		if !ok {
			return nil
		}
		length = length<<4 | int(v)
	}
	if length < 87 || length+4 > len(body) {
		return nil
	}
	if string(body[length:length+4]) != pktFlushLine {
		return nil // more than one command packet or no flush
	}
	line := body[4:length]
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	nul := bytes.IndexByte(line, 0)
	var capBytes []byte
	if nul >= 0 {
		capBytes = line[nul+1:]
		line = line[:nul]
	}
	if len(line) < 83 || line[40] != ' ' || line[81] != ' ' {
		return nil
	}
	oldBytes, newBytes, refBytes := line[:40], line[41:81], line[82:]
	if !isOIDBytes(oldBytes) || !isOIDBytes(newBytes) || string(newBytes) == ZeroOID {
		return nil
	}
	for _, ch := range refBytes {
		if ch <= 0x20 || ch >= 0x7f {
			return nil
		}
	}
	wantsReport, useSideband := false, false
	for len(capBytes) > 0 {
		tok := capBytes
		if sp := bytes.IndexByte(capBytes, ' '); sp >= 0 {
			tok, capBytes = capBytes[:sp], capBytes[sp+1:]
		} else {
			capBytes = nil
		}
		switch string(tok) {
		case "report-status", "report-status-v2":
			wantsReport = true
		case "side-band-64k":
			useSideband = true
		}
	}
	packData := body[length+4:]
	if len(packData) == 0 {
		return nil
	}
	objects, err := ParsePack(packData, func(oid string) *GitObject {
		g.Mu.RLock()
		defer g.Mu.RUnlock()
		return g.objects[oid]
	})
	if err != nil {
		return nil // let general path format unpack error
	}
	cmd := pushCommand{oldOID: string(oldBytes), newOID: string(newBytes), ref: string(refBytes)}
	if cmd.ref == "refs/heads/main" {
		cmd.ref = "refs/heads/main" // intern constant string, no heap copy needed if matched
	}

	storeMu.Lock()
	g.Mu.Lock()
	for _, obj := range objects {
		g.AddObject(obj)
	}
	linkProblem := g.missingLink(objects)
	verdict := g.pushVerdict(cmd, "ok", linkProblem)
	if verdict == "" {
		hadBranches := g.hasRefWithPrefix("refs/heads/")
		g.SetRef(cmd.ref, cmd.newOID)
		if !hadBranches && strings.HasPrefix(cmd.ref, "refs/heads/") && !g.HasRef(g.HeadRefName()) {
			g.DefaultBranch = cmd.ref[len("refs/heads/"):]
		}
	}
	g.Mu.Unlock()
	storeMu.Unlock()

	if verdict == "" && cmd.ref == "refs/heads/main" && wantsReport && useSideband {
		return okMainSidebandFrame
	}
	if !wantsReport {
		if useSideband {
			return HTTPFrame(200, ctReceiveResult, GitNoCacheHeaders, []byte(pktFlushLine))
		}
		return HTTPFrame(200, ctReceiveResult, GitNoCacheHeaders, nil)
	}
	var report []byte
	report = appendPkt(report, "unpack ok\n")
	if verdict == "" {
		report = appendPkt(report, "ok ", cmd.ref, "\n")
	} else {
		report = appendPkt(report, "ng ", cmd.ref, " ", verdict, "\n")
	}
	report = append(report, pktFlushLine...)
	if !useSideband {
		return HTTPFrame(200, ctReceiveResult, GitNoCacheHeaders, report)
	}
	out := appendSideband(make([]byte, 0, len(report)+16), report, 1)
	out = append(out, pktFlushLine...)
	return HTTPFrame(200, ctReceiveResult, GitNoCacheHeaders, out)
}

func queryServiceBytes(query []byte) string {
	for {
		part, rest, more := bytes.Cut(query, []byte("&"))
		switch string(part) {
		case "service=git-upload-pack":
			return "git-upload-pack"
		case "service=git-receive-pack":
			return "git-receive-pack"
		}
		if !more {
			return ""
		}
		query = rest
	}
}

func wantsV2Bytes(header []byte) bool {
	for len(header) > 0 {
		part, rest, more := bytes.Cut(header, []byte(":"))
		if string(part) == "version=2" {
			return true
		}
		if !more {
			return false
		}
		header = rest
	}
	return false
}
// /(?:^|&)service=(git-upload-pack|git-receive-pack)(?:&|$)/ does; "" when there is none.
func queryService(query string) string {
	for {
		part, rest, more := strings.Cut(query, "&")
		switch part {
		case "service=git-upload-pack":
			return "git-upload-pack"
		case "service=git-receive-pack":
			return "git-receive-pack"
		}
		if !more {
			return ""
		}
		query = rest
	}
}

// wantsV2 reports whether the Git-Protocol header, split on ":", contains "version=2".
func wantsV2(header string) bool {
	for {
		part, rest, more := strings.Cut(header, ":")
		if part == "version=2" {
			return true
		}
		if !more {
			return false
		}
		header = rest
	}
}

// jsQuote is JSON.stringify of a string: quotes, backslash and control characters escaped, the rest verbatim.
func jsQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hexDigits[r>>4])
				b.WriteByte(hexDigits[r&15])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// latin1 decodes bytes one character per byte, as Buffer.toString('latin1') does.
func latin1(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

// utf16Prefix is s.slice(0, n) in UTF-16 code units (never splitting a surrogate pair).
func utf16Prefix(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}
