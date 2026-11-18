package main

// Git object primitives: hashing, trees, commits, tags and the lazily deflated pack payload every stored
// object carries. Everything is in memory and built on the Go standard library only.
//
// Concurrency: a *GitObject is immutable once published (OID, Type, Data never change). Its three caches —
// the deflated stream, the parsed commit and the parsed tree — are atomic pointers filled on first use, so any
// number of goroutines (e.g. 50 concurrent clones) may read the same object without locks.

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

const (
	ZeroOID      = "0000000000000000000000000000000000000000"
	EmptyTreeOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// ObjType is a git object type. Its value is the pack type code (commit 1, tree 2, blob 3, tag 4).
type ObjType uint8

const (
	ObjCommit ObjType = 1
	ObjTree   ObjType = 2
	ObjBlob   ObjType = 3
	ObjTag    ObjType = 4
)

// String is the canonical type name ("commit", "tree", "blob", "tag"), or "" for an invalid type.
func (t ObjType) String() string {
	switch t {
	case ObjCommit:
		return "commit"
	case ObjTree:
		return "tree"
	case ObjBlob:
		return "blob"
	case ObjTag:
		return "tag"
	}
	return ""
}

// ParseObjType maps a type name to its ObjType.
func ParseObjType(name string) (ObjType, bool) {
	switch name {
	case "commit":
		return ObjCommit, true
	case "tree":
		return ObjTree, true
	case "blob":
		return ObjBlob, true
	case "tag":
		return ObjTag, true
	}
	return 0, false
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

type Ident struct {
	Name  string
	Email string
	When  int64
	TZ    string
}

type CommitInfo struct {
	Tree      string
	Parents   []string
	Author    Ident
	Committer Ident
	// Message is the message exactly as stored in the object.
	Message string
}

type TagInfo struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

// GitObject is one stored object. Always handle it by pointer.
type GitObject struct {
	OID  string
	Type ObjType
	Data []byte

	// deflated is the zlib stream of Data (level 1), built lazily by Deflated the first time an outgoing pack
	// carries the object and then reused by every later pack. Objects ingested by receive-pack start with
	// it empty: nothing is compressed until somebody fetches.
	deflated atomic.Pointer[[]byte]
	commit   atomic.Pointer[CommitInfo]
	tree     atomic.Pointer[[]TreeEntry]
}

// HashObject is the object id: SHA-1 over "<type> <size>\x00" followed by the data.
func HashObject(t ObjType, data []byte) string {
	var sum [sha1.Size]byte
	if len(data) <= 480 {
		// Small objects (commits, most trees): frame them in a stack buffer and hash in one sha1.Sum.
		var buf [512]byte
		framed := appendObjectHeader(buf[:0], t, len(data))
		framed = append(framed, data...)
		sum = sha1.Sum(framed)
	} else {
		var head [32]byte
		d := sha1.New()
		d.Write(appendObjectHeader(head[:0], t, len(data)))
		d.Write(data)
		d.Sum(sum[:0])
	}
	return oidHex(&sum)
}

// appendObjectHeader appends the loose-object header "<type> <size>\x00".
func appendObjectHeader(dst []byte, t ObjType, size int) []byte {
	dst = append(dst, t.String()...)
	dst = append(dst, ' ')
	dst = strconv.AppendInt(dst, int64(size), 10)
	return append(dst, 0)
}

func oidHex(sum *[sha1.Size]byte) string {
	var buf [2 * sha1.Size]byte
	hex.Encode(buf[:], sum[:])
	return string(buf[:])
}

// MakeObject hashes data and wraps it as an object (nothing is deflated yet).
func MakeObject(t ObjType, data []byte) *GitObject {
	return &GitObject{OID: HashObject(t, data), Type: t, Data: data}
}

// IsOID reports whether value is a full lowercase 40-hex object id.
func IsOID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// AppendEntryHeader appends the pack entry header: the type in bits 6-4 of the first byte, then the size as a
// little-endian base-128 varint (4 bits in the first byte, 7 in each following one).
func AppendEntryHeader(dst []byte, t ObjType, size int) []byte {
	first := byte(t)<<4 | byte(size&15)
	rest := uint64(size) >> 4
	for rest > 0 {
		dst = append(dst, first|0x80)
		first = byte(rest & 0x7f)
		rest >>= 7
	}
	return append(dst, first)
}

// entryHeaderLen is len(AppendEntryHeader(nil, t, size)).
func entryHeaderLen(size int) int {
	n := 1
	for rest := uint64(size) >> 4; rest > 0; rest >>= 7 {
		n++
	}
	return n
}

// Deflated returns the object's zlib stream, compressing it on first use (pooled writer, level 1) and caching it.
// Concurrent first calls may both compress; the bytes are identical and the first stored copy wins.
func (o *GitObject) Deflated() []byte {
	if d := o.deflated.Load(); d != nil {
		return *d
	}
	z := deflateBytes(o.Data)
	if o.deflated.CompareAndSwap(nil, &z) {
		return z
	}
	return *o.deflated.Load()
}

// IsDeflated reports whether the deflated stream is already cached.
func (o *GitObject) IsDeflated() bool {
	return o.deflated.Load() != nil
}

// Commit is the parsed commit, cached on the object (only meaningful for commit objects).
func (o *GitObject) Commit() *CommitInfo {
	if c := o.commit.Load(); c != nil {
		return c
	}
	c := ParseCommit(o.Data)
	if o.commit.CompareAndSwap(nil, c) {
		return c
	}
	return o.commit.Load()
}

// Tree is the parsed entry list, cached on the object. It is shared: callers must not modify it. A malformed
// tree yields the entries before the damage, and a non-tree object yields none.
func (o *GitObject) Tree() []TreeEntry {
	if t := o.tree.Load(); t != nil {
		return *t
	}
	var entries []TreeEntry
	if o.Type == ObjTree {
		entries, _ = ParseTree(o.Data)
	}
	if entries == nil {
		entries = []TreeEntry{}
	}
	if o.tree.CompareAndSwap(nil, &entries) {
		return entries
	}
	return *o.tree.Load()
}

// TreeChecked parses the data strictly (as receive-pack's link check does) and, for a valid tree, fills the cache.
func (o *GitObject) TreeChecked() ([]TreeEntry, error) {
	entries, err := ParseTree(o.Data)
	if err != nil || o.Type != ObjTree {
		return entries, err
	}
	if o.tree.CompareAndSwap(nil, &entries) {
		return entries, nil
	}
	return *o.tree.Load(), nil
}

// ---------------------------------------------------------------- trees

var errMalformedTree = errors.New("malformed tree object")

// ParseTree splits tree data into entries. All names and modes share one copy of the data and all object ids
// share one hex string, so a tree costs three allocations however many entries it has.
func ParseTree(data []byte) ([]TreeEntry, error) {
	count := 0
	var bad error
	for pos := 0; pos < len(data); {
		space := bytes.IndexByte(data[pos:], ' ')
		if space < 0 {
			bad = errMalformedTree
			break
		}
		space += pos
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			bad = errMalformedTree
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			bad = errMalformedTree
			break
		}
		count++
		pos = nul + 21
	}
	entries := make([]TreeEntry, 0, count)
	if count == 0 {
		return entries, bad
	}
	text := string(data)
	var hexes strings.Builder
	hexes.Grow(40 * count)
	var tmp [40]byte
	pos := 0
	for i := 0; i < count; i++ {
		space := pos + strings.IndexByte(text[pos:], ' ')
		nul := space + 1 + strings.IndexByte(text[space+1:], 0)
		hex.Encode(tmp[:], data[nul+1:nul+21])
		hexes.Write(tmp[:])
		entries = append(entries, TreeEntry{Mode: text[pos:space], Name: text[space+1 : nul]})
		pos = nul + 21
	}
	all := hexes.String()
	for i := range entries {
		entries[i].OID = all[40*i : 40*i+40]
	}
	return entries, bad
}

// treeKeyByte is byte i of the entry's sort key (its name, plus "/" for a directory), or -1 past the end.
func treeKeyByte(e *TreeEntry, i int) int {
	if i < len(e.Name) {
		return int(e.Name[i])
	}
	if i == len(e.Name) && e.Mode == "40000" {
		return '/'
	}
	return -1
}

// compareTreeEntries orders entries the way git sorts a tree: bytewise by name, directories as if "name/".
func compareTreeEntries(a, b TreeEntry) int {
	n := min(len(a.Name), len(b.Name))
	if c := strings.Compare(a.Name[:n], b.Name[:n]); c != 0 {
		return c
	}
	for i := n; ; i++ {
		x, y := treeKeyByte(&a, i), treeKeyByte(&b, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
		if x < 0 {
			return 0
		}
	}
}

// BuildTree serializes entries in git order (stable for equal keys). The input slice is not modified.
func BuildTree(entries []TreeEntry) []byte {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, compareTreeEntries)
	size := 0
	for i := range sorted {
		size += len(sorted[i].Mode) + len(sorted[i].Name) + 22
	}
	out := make([]byte, 0, size)
	for i := range sorted {
		out = append(out, sorted[i].Mode...)
		out = append(out, ' ')
		out = append(out, sorted[i].Name...)
		out = append(out, 0)
		out = appendHexBytes(out, sorted[i].OID)
	}
	return out
}

// appendHexBytes decodes hex the way Buffer.from(s, 'hex') does: pair by pair, stopping at the first bad pair.
func appendHexBytes(dst []byte, s string) []byte {
	for i := 0; i+1 < len(s); i += 2 {
		hi, ok1 := hexNibble(s[i])
		lo, ok2 := hexNibble(s[i+1])
		if !ok1 || !ok2 {
			break
		}
		dst = append(dst, hi<<4|lo)
	}
	return dst
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// ---------------------------------------------------------------- commits

func emptyIdent() Ident {
	return Ident{TZ: "+0000"}
}

// ParseIdent parses "Name <email> 1234567890 +0000". It matches exactly what the regex
// /^(.*) <(.*)> (-?\d+) ([+-]\d{4})$/ matches (greedy groups, "." excluding line terminators); anything else
// becomes {name: line, email: "", when: 0, tz: "+0000"}.
func ParseIdent(line string) Ident {
	if id, ok := matchIdent(line); ok {
		return id
	}
	return Ident{Name: line, TZ: "+0000"}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func matchIdent(s string) (Ident, bool) {
	n := len(s)
	// ([+-]\d{4})$, preceded by a space.
	tz := n - 5
	if tz < 1 || (s[tz] != '+' && s[tz] != '-') || s[tz-1] != ' ' {
		return Ident{}, false
	}
	for i := tz + 1; i < n; i++ {
		if !isDigit(s[i]) {
			return Ident{}, false
		}
	}
	// (-?\d+) right before that space: the digit run is maximal, since only "> " may precede it.
	digitsEnd := tz - 1
	q := digitsEnd
	for q > 0 && isDigit(s[q-1]) {
		q--
	}
	if q == digitsEnd {
		return Ident{}, false
	}
	numStart := q
	if q > 0 && s[q-1] == '-' {
		numStart = q - 1
	}
	if numStart < 2 || s[numStart-1] != ' ' || s[numStart-2] != '>' {
		return Ident{}, false
	}
	gt := numStart - 2
	left := s[:gt]
	// (.*) <(.*): the greedy first group ends at the last " <".
	lt := strings.LastIndex(left, " <")
	if lt < 0 {
		return Ident{}, false
	}
	if strings.ContainsAny(left, "\r\n  ") {
		return Ident{}, false
	}
	return Ident{Name: left[:lt], Email: left[lt+2:], When: parseWhen(s[numStart:digitsEnd]), TZ: s[tz:]}, true
}

// parseWhen is Number() of an optional minus and digits, saturated to int64 when it is out of range.
func parseWhen(digits string) int64 {
	if v, err := strconv.ParseInt(digits, 10, 64); err == nil {
		return v
	}
	if f, err := strconv.ParseFloat(digits, 64); err == nil {
		if f > 9.2e18 {
			return 1<<63 - 1
		}
		if f < -9.2e18 {
			return -1 << 63
		}
		return int64(f)
	}
	return 0
}

func FormatIdent(id Ident) string {
	var b []byte
	b = appendIdent(b, id)
	return string(b)
}

func appendIdent(dst []byte, id Ident) []byte {
	dst = append(dst, id.Name...)
	dst = append(dst, " <"...)
	dst = append(dst, id.Email...)
	dst = append(dst, "> "...)
	dst = strconv.AppendInt(dst, id.When, 10)
	dst = append(dst, ' ')
	return append(dst, id.TZ...)
}

// splitHead splits object text at the first blank line into header and message (no blank line: no message).
func splitHead(text string) (head, message string) {
	if i := strings.Index(text, "\n\n"); i >= 0 {
		return text[:i], text[i+2:]
	}
	return text, ""
}

// ParseCommit reads tree/parent/author/committer header lines; every other header line is ignored and a
// missing ident stays {"" "" 0 "+0000"}. All strings share one copy of the data.
func ParseCommit(data []byte) *CommitInfo {
	head, message := splitHead(string(data))
	info := &CommitInfo{Parents: []string{}, Author: emptyIdent(), Committer: emptyIdent(), Message: message}
	for {
		line, rest, more := strings.Cut(head, "\n")
		switch {
		case strings.HasPrefix(line, "tree "):
			info.Tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			info.Parents = append(info.Parents, line[7:])
		case strings.HasPrefix(line, "author "):
			info.Author = ParseIdent(line[7:])
		case strings.HasPrefix(line, "committer "):
			info.Committer = ParseIdent(line[10:])
		}
		if !more {
			break
		}
		head = rest
	}
	return info
}

// BuildCommit serializes a commit: tree, parents, author, committer, a blank line, then the message verbatim.
func BuildCommit(tree string, parents []string, author, committer Ident, message string) []byte {
	size := 6 + len(tree) + 1 + 48*len(parents) + 2*96 + len(author.Name) + len(author.Email) + len(committer.Name) + len(committer.Email) + 2 + len(message)
	out := make([]byte, 0, size)
	out = append(out, "tree "...)
	out = append(out, tree...)
	out = append(out, '\n')
	for _, parent := range parents {
		out = append(out, "parent "...)
		out = append(out, parent...)
		out = append(out, '\n')
	}
	out = append(out, "author "...)
	out = appendIdent(out, author)
	out = append(out, "\ncommitter "...)
	out = appendIdent(out, committer)
	out = append(out, "\n\n"...)
	return append(out, message...)
}

// ParseTag reads object/type/tag/tagger header lines and the message after the first blank line.
func ParseTag(data []byte) TagInfo {
	head, message := splitHead(string(data))
	info := TagInfo{Message: message}
	for {
		line, rest, more := strings.Cut(head, "\n")
		switch {
		case strings.HasPrefix(line, "object "):
			info.Object = line[7:]
		case strings.HasPrefix(line, "type "):
			info.Type = line[5:]
		case strings.HasPrefix(line, "tag "):
			info.Tag = line[4:]
		case strings.HasPrefix(line, "tagger "):
			info.Tagger = line[7:]
		}
		if !more {
			break
		}
		head = rest
	}
	return info
}

// ---------------------------------------------------------------- ref names

// IsRefNameValid applies the forge's ref-name rules: under refs/, at most 250 UTF-16 code units, no "..", "//",
// "@{" or backslash, no trailing "/", "." or ".lock", and none of control characters, space, DEL, ~ ^ : ? * [.
func IsRefNameValid(name string) bool {
	if !strings.HasPrefix(name, "refs/") || utf16Len(name) > 250 {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") {
		return false
	}
	if strings.Contains(name, "@{") || strings.Contains(name, "\\") {
		return false
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c <= 0x20, c == 0x7f, c == '~', c == '^', c == ':', c == '?', c == '*', c == '[':
			return false
		}
	}
	return true
}

// utf16Len is the JavaScript .length of s (invalid bytes count as one U+FFFD each).
func utf16Len(s string) int {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return len(s)
	}
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}
