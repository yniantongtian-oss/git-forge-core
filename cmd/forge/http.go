package main

// The HTTP surface: a raw-TCP HTTP/1.1 keep-alive server (one goroutine per connection, a reusable read buffer,
// byte-level request parsing) that routes git smart HTTP, the REST v3 API, GraphQL and MCP exactly as app.ts
// does, checks the bearer token, stamps X-GitHub-Request-Id on JSON replies and turns every failure into a JSON
// body. GET /api/v3/repos/{owner}/{repo}/issues/{n} is answered from the issue's pre-rendered reply with no heap
// allocation. Also home to the helpers every surface shares (util.ts): ordered JSON writing, JSON parsing, query
// parsing and the JavaScript-compatible string conversions.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------- ordered JSON

// Obj is a JSON object that serializes its keys in insertion order (as JSON.stringify does).
type Obj struct {
	keys []string
	vals []any
}

// O builds an object from alternating keys and values (keys must be distinct).
func O(kv ...any) *Obj {
	o := &Obj{keys: make([]string, 0, len(kv)/2), vals: make([]any, 0, len(kv)/2)}
	for i := 0; i+1 < len(kv); i += 2 {
		o.keys = append(o.keys, kv[i].(string))
		o.vals = append(o.vals, kv[i+1])
	}
	return o
}

// Set replaces an existing key in place or appends a new one (object spread semantics).
func (o *Obj) Set(key string, value any) *Obj {
	for i, k := range o.keys {
		if k == key {
			o.vals[i] = value
			return o
		}
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, value)
	return o
}

func (o *Obj) Get(key string) (any, bool) {
	for i, k := range o.keys {
		if k == key {
			return o.vals[i], true
		}
	}
	return nil, false
}

// RawJSON is inserted into the output verbatim.
type RawJSON []byte

// jsUndefined is JavaScript's undefined: omitted from objects, null in arrays.
type jsUndefined struct{}

var undefined = jsUndefined{}

func strOrNil(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

const upperHex = "0123456789ABCDEF"

// appendJSONString writes s as JSON.stringify does (no HTML escaping); invalid UTF-8 becomes U+FFFD.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < utf8.RuneSelf {
			i++
			continue
		}
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				dst = append(dst, s[start:i]...)
				dst = append(dst, "�"...)
				i++
				start = i
				continue
			}
			i += size
			continue
		}
		dst = append(dst, s[start:i]...)
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&15])
		}
		i++
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// jsNumberString is Number.prototype.toString for a float64.
func jsNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	if abs := math.Abs(f); abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	mantissa, exp, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	digits := strings.TrimLeft(exp[1:], "0")
	if digits == "" {
		digits = "0"
	}
	return mantissa + "e" + exp[:1] + digits
}

func isSkippable(v any) bool {
	switch v.(type) {
	case jsUndefined, lazyField:
		return true
	}
	return false
}

// appendJSON serializes v with JSON.stringify semantics, without reflection.
func appendJSON(dst []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if x {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case string:
		return appendJSONString(dst, x)
	case *string:
		if x == nil {
			return append(dst, "null"...)
		}
		return appendJSONString(dst, *x)
	case int:
		return strconv.AppendInt(dst, int64(x), 10)
	case int64:
		return strconv.AppendInt(dst, x, 10)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return append(dst, "null"...)
		}
		return append(dst, jsNumberString(x)...)
	case RawJSON:
		return append(dst, x...)
	case *Obj:
		if x == nil {
			return append(dst, "null"...)
		}
		dst = append(dst, '{')
		first := true
		for i, key := range x.keys {
			if isSkippable(x.vals[i]) {
				continue
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = appendJSONString(dst, key)
			dst = append(dst, ':')
			dst = appendJSON(dst, x.vals[i])
		}
		return append(dst, '}')
	case []any:
		dst = append(dst, '[')
		for i, item := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			if isSkippable(item) {
				dst = append(dst, "null"...)
			} else {
				dst = appendJSON(dst, item)
			}
		}
		return append(dst, ']')
	case []string:
		dst = append(dst, '[')
		for i, item := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, item)
		}
		return append(dst, ']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for key := range x {
			if !isSkippable(x[key]) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		dst = append(dst, '{')
		for i, key := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, key)
			dst = append(dst, ':')
			dst = appendJSON(dst, x[key])
		}
		return append(dst, '}')
	}
	return append(dst, "null"...)
}

// parseJSON is JSON.parse: numbers become float64, objects map[string]any, arrays []any.
func parseJSON(data []byte) (any, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// jsString is String(value).
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case jsUndefined:
		return "undefined"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case []any:
		parts := make([]string, len(x))
		for i, item := range x {
			if item != nil && item != any(undefined) {
				parts[i] = jsString(item)
			}
		}
		return strings.Join(parts, ",")
	case error:
		return x.Error()
	}
	return "[object Object]"
}

// ---------------------------------------------------------------- query strings and URI components

type query struct {
	keys []string
	vals map[string]string
}

// parseQuery splits a query string into a map (the last value of a repeated key wins, at its first position).
func parseQuery(search string) *query {
	q := &query{vals: map[string]string{}}
	if search == "" {
		return q
	}
	for _, part := range strings.Split(search, "&") {
		if part == "" {
			continue
		}
		rawKey, rawValue, hasValue := strings.Cut(part, "=")
		key := jsDecode(rawKey)
		value := ""
		if hasValue {
			value = jsDecode(rawValue)
		}
		if _, exists := q.vals[key]; !exists {
			q.keys = append(q.keys, key)
		}
		q.vals[key] = value
	}
	return q
}

func (q *query) get(key string) (string, bool) {
	v, ok := q.vals[key]
	return v, ok
}

func isArrayIndex(key string) bool {
	if key == "" || len(key) > 10 || (len(key) > 1 && key[0] == '0') {
		return false
	}
	for i := 0; i < len(key); i++ {
		if !isDigit(key[i]) {
			return false
		}
	}
	n, err := strconv.ParseUint(key, 10, 64)
	return err == nil && n < 1<<32-1
}

// objectKeys is Object.keys order: array-index keys ascending, then the rest in insertion order.
func (q *query) objectKeys() []string {
	var indexes, others []string
	for _, key := range q.keys {
		if isArrayIndex(key) {
			indexes = append(indexes, key)
		} else {
			others = append(others, key)
		}
	}
	sort.Slice(indexes, func(i, j int) bool {
		a, _ := strconv.ParseUint(indexes[i], 10, 64)
		b, _ := strconv.ParseUint(indexes[j], 10, 64)
		return a < b
	})
	return append(indexes, others...)
}

// jsDecode is decodeURIComponent(text.replace(/\+/g, ' ')), or text unchanged when that throws.
func jsDecode(text string) string {
	if strings.IndexByte(text, '%') < 0 && strings.IndexByte(text, '+') < 0 {
		return text
	}
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch c {
		case '+':
			out = append(out, ' ')
		case '%':
			if i+2 >= len(text) {
				return text
			}
			hi, ok1 := hexNibble(text[i+1])
			lo, ok2 := hexNibble(text[i+2])
			if !ok1 || !ok2 {
				return text
			}
			out = append(out, hi<<4|lo)
			i += 2
		default:
			out = append(out, c)
		}
	}
	if !utf8.Valid(out) {
		return text
	}
	return string(out)
}

// jsEncodeURIComponent is encodeURIComponent.
func jsEncodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&15])
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- base URL and request ids

var (
	baseMu       sync.Mutex
	apiBase      atomic.Pointer[string]
	apiBaseKnown atomic.Bool
)

func init() {
	initial := "http://localhost"
	apiBase.Store(&initial)
}

// setBase remembers the host the suite reaches the forge at (first one wins), so URLs in answers point back at it.
func setBase(host []byte) {
	if apiBaseKnown.Load() || len(host) == 0 {
		return
	}
	baseMu.Lock()
	if !apiBaseKnown.Load() {
		s := "http://" + string(host)
		apiBase.Store(&s)
		apiBaseKnown.Store(true)
	}
	baseMu.Unlock()
}

func base() string { return *apiBase.Load() }

var requestCounter atomic.Uint64

// appendRequestID writes the GitHub-shaped request id for reply number n.
func appendRequestID(dst []byte, n uint64) []byte {
	var tmp [16]byte
	i := len(tmp)
	for v := n; v > 0 || len(tmp)-i < 8; v >>= 4 {
		i--
		tmp[i] = upperHex[v&15]
	}
	h := tmp[i:]
	dst = append(dst, "FEED:"...)
	dst = append(dst, h[:4]...)
	dst = append(dst, ':')
	dst = append(dst, h[4:]...)
	return append(dst, ":0000A1:00000001"...)
}

const jsonContentType = "application/json; charset=utf-8"

// jsonFrame is sendJson: Content-Type, Content-Length, a fresh X-GitHub-Request-Id, then extra header lines.
func jsonFrame(status int, body []byte, extra string) []byte {
	var rid [96]byte
	h := append(rid[:0], "X-GitHub-Request-Id: "...)
	h = appendRequestID(h, requestCounter.Add(1))
	h = append(h, "\r\n"...)
	return HTTPFrame(status, jsonContentType, string(h)+extra, body)
}

func jsonValueFrame(status int, v any, extra string) []byte {
	return jsonFrame(status, appendJSON(make([]byte, 0, 1024), v), extra)
}

// errorFrame is sendError: {message, errors?, documentation_url}.
func errorFrame(status int, message string, errs []any, extra string) []byte {
	o := O("message", message)
	if errs != nil {
		o.Set("errors", errs)
	}
	o.Set("documentation_url", "https://docs.github.com/rest")
	return jsonValueFrame(status, o, extra)
}

// ---------------------------------------------------------------- auth

var (
	shopToken  = os.Getenv("MAKEFUN_SHOP_TOKEN")
	bearerLine = []byte("Bearer " + shopToken)
)

// authorized: `Authorization: Bearer <token>` (or `token <token>`) naming MAKEFUN_SHOP_TOKEN; without a
// configured token any header longer than 7 characters passes.
func authorized(header []byte) bool {
	if len(header) == 0 {
		return false
	}
	if shopToken == "" {
		return len(header) > 7
	}
	if bytes.Equal(header, bearerLine) {
		return true
	}
	space := bytes.IndexByte(header, ' ')
	scheme := header[:len(header)-1]
	if space >= 0 {
		scheme = header[:space]
	}
	if !asciiEqualFold(scheme, "bearer") && !asciiEqualFold(scheme, "token") {
		return false
	}
	return string(header[len(scheme)+1:]) == shopToken
}

func asciiEqualFold(b []byte, lower string) bool {
	if len(b) != len(lower) {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- the server

const (
	maxHeaderBytes = 64 << 10
	maxBodyBytes   = 512 << 20
)

type httpRequest struct {
	method          []byte
	path            []byte
	query           []byte
	host            []byte
	hasHost         bool
	auth            []byte
	accept          []byte
	gitProtocol     []byte
	contentEncoding []byte
	contentLength   int64
	hasLength       bool
	chunked         bool
	expectContinue  bool
	keepAlive       bool
	body            []byte
}

type httpConn struct {
	nc  net.Conn
	buf []byte
	r   int // start of unread data
	w   int // end of unread data
	out []byte
	key []byte
}

// serveHTTP accepts connections until the listener closes.
func serveHTTP(ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if tc, ok := nc.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		go (&httpConn{nc: nc}).serve()
	}
}

func (c *httpConn) serve() {
	defer c.nc.Close()
	c.buf = make([]byte, 16<<10)
	c.out = make([]byte, 0, 8<<10)
	c.key = make([]byte, 0, 128)
	var req httpRequest
	for {
		req = httpRequest{}
		status := c.readRequest(&req)
		if status == -1 {
			return
		}
		if status != 0 {
			message := StatusText(status)
			if status == 413 {
				message = "Request body too large"
			}
			c.nc.Write(errorFrame(status, message, nil, "Connection: close\r\n"))
			return
		}
		if !c.handle(&req) || !req.keepAlive {
			return
		}
		if c.r == c.w {
			c.r, c.w = 0, 0
			if len(c.buf) > 64<<10 {
				c.buf = make([]byte, 16<<10)
			}
		}
	}
}

// fill reads more bytes into the buffer (compacting or growing it first). It returns false on EOF/error. No read
// deadlines: keep-alive reads never touch the runtime timer heap.
func (c *httpConn) fill() bool {
	if c.r > 0 && (c.w == len(c.buf) || c.r == c.w) {
		copy(c.buf, c.buf[c.r:c.w])
		c.w -= c.r
		c.r = 0
	}
	if c.w == len(c.buf) {
		grown := make([]byte, 2*len(c.buf))
		copy(grown, c.buf[:c.w])
		c.buf = grown
	}
	n, err := c.nc.Read(c.buf[c.w:])
	c.w += n
	return n > 0 || err == nil
}

// readRequest parses one request (head and body). It returns 0 on success, -1 to drop the connection, or an
// HTTP status to answer with before closing.
func (c *httpConn) readRequest(req *httpRequest) int {
	headEnd := -1
	for {
		for c.w-c.r >= 2 && c.buf[c.r] == '\r' && c.buf[c.r+1] == '\n' {
			c.r += 2 // stray CRLF between pipelined requests
		}
		if i := bytes.Index(c.buf[c.r:c.w], []byte("\r\n\r\n")); i >= 0 {
			headEnd = c.r + i + 4
			break
		}
		if c.w-c.r >= maxHeaderBytes {
			return 431
		}
		if !c.fill() {
			return -1
		}
	}
	head := c.buf[c.r:headEnd]
	lineEnd := bytes.Index(head, []byte("\r\n"))
	line := head[:lineEnd]
	sp1 := bytes.IndexByte(line, ' ')
	sp2 := bytes.LastIndexByte(line, ' ')
	if sp1 <= 0 || sp2 <= sp1 {
		return 400
	}
	req.method = line[:sp1]
	target := line[sp1+1 : sp2]
	version := line[sp2+1:]
	if q := bytes.IndexByte(target, '?'); q >= 0 {
		req.path, req.query = target[:q], target[q+1:]
	} else {
		req.path = target
	}
	req.keepAlive = !bytes.Equal(version, []byte("HTTP/1.0"))
	rest := head[lineEnd+2:]
	for len(rest) > 2 {
		end := bytes.Index(rest, []byte("\r\n"))
		if end < 0 {
			break
		}
		field := rest[:end]
		rest = rest[end+2:]
		colon := bytes.IndexByte(field, ':')
		if colon <= 0 {
			continue
		}
		name := field[:colon]
		value := bytes.Trim(field[colon+1:], " \t")
		switch {
		case asciiEqualFold(name, "host"):
			if !req.hasHost {
				req.host, req.hasHost = value, true
			}
		case asciiEqualFold(name, "authorization"):
			if req.auth == nil {
				req.auth = value
			}
		case asciiEqualFold(name, "content-length"):
			if !req.hasLength {
				n, err := strconv.ParseInt(string(value), 10, 64)
				if err != nil || n < 0 {
					return 400
				}
				req.contentLength, req.hasLength = n, true
			}
		case asciiEqualFold(name, "transfer-encoding"):
			if bytes.Contains(bytes.ToLower(value), []byte("chunked")) {
				req.chunked = true
			}
		case asciiEqualFold(name, "content-encoding"):
			if req.contentEncoding == nil {
				req.contentEncoding = value
			}
		case asciiEqualFold(name, "connection"):
			switch {
			case asciiEqualFold(value, "keep-alive"): // what undici sends on every request
				req.keepAlive = true
			case asciiEqualFold(value, "close"):
				req.keepAlive = false
			default: // a token list, scanned in place
				for list := value; len(list) > 0; {
					token := list
					if comma := bytes.IndexByte(list, ','); comma >= 0 {
						token, list = list[:comma], list[comma+1:]
					} else {
						list = nil
					}
					token = bytes.TrimSpace(token)
					if asciiEqualFold(token, "close") {
						req.keepAlive = false
					} else if asciiEqualFold(token, "keep-alive") {
						req.keepAlive = true
					}
				}
			}
		case asciiEqualFold(name, "git-protocol"):
			req.gitProtocol = joinHeader(req.gitProtocol, value)
		case asciiEqualFold(name, "accept"):
			req.accept = joinHeader(req.accept, value)
		case asciiEqualFold(name, "expect"):
			req.expectContinue = asciiEqualFold(value, "100-continue")
		}
	}
	c.r = headEnd
	if req.chunked {
		// The body may move the buffer around: keep private copies of everything the handlers read.
		req.method, req.path, req.query = bytes.Clone(req.method), bytes.Clone(req.path), bytes.Clone(req.query)
		req.host, req.auth, req.accept = bytes.Clone(req.host), bytes.Clone(req.auth), bytes.Clone(req.accept)
		req.gitProtocol, req.contentEncoding = bytes.Clone(req.gitProtocol), bytes.Clone(req.contentEncoding)
		if req.expectContinue {
			c.nc.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		}
		body, status := c.readChunked()
		if status != 0 {
			return status
		}
		req.body = body
		return 0
	}
	if !req.hasLength || req.contentLength == 0 {
		return 0
	}
	if req.contentLength > maxBodyBytes {
		return 413
	}
	n := int(req.contentLength)
	if c.w-c.r >= n {
		req.body = c.buf[c.r : c.r+n]
		c.r += n
		return 0
	}
	if req.expectContinue {
		c.nc.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}
	body := make([]byte, n)
	have := copy(body, c.buf[c.r:c.w])
	c.r, c.w = 0, 0
	if _, err := io.ReadFull(c.nc, body[have:]); err != nil {
		return -1
	}
	req.body = body
	return 0
}

func joinHeader(existing, value []byte) []byte {
	if existing == nil {
		return value
	}
	joined := make([]byte, 0, len(existing)+2+len(value))
	joined = append(joined, existing...)
	joined = append(joined, ", "...)
	return append(joined, value...)
}

// readLine returns the next CRLF-terminated line (without the CRLF), reading as needed.
func (c *httpConn) readLine() ([]byte, bool) {
	for {
		if i := bytes.Index(c.buf[c.r:c.w], []byte("\r\n")); i >= 0 {
			line := c.buf[c.r : c.r+i]
			c.r += i + 2
			return line, true
		}
		if c.w-c.r > maxHeaderBytes || !c.fill() {
			return nil, false
		}
	}
}

func (c *httpConn) readChunked() ([]byte, int) {
	body := []byte{}
	for {
		line, ok := c.readLine()
		if !ok {
			return nil, -1
		}
		if semi := bytes.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size, err := strconv.ParseInt(string(bytes.TrimSpace(line)), 16, 64)
		if err != nil || size < 0 {
			return nil, 400
		}
		if size == 0 {
			for { // trailers, up to the empty line
				trailer, ok := c.readLine()
				if !ok {
					return nil, -1
				}
				if len(trailer) == 0 {
					return body, 0
				}
			}
		}
		if int64(len(body))+size > maxBodyBytes {
			return nil, 413
		}
		for remaining := int(size); remaining > 0; {
			if c.r == c.w && !c.fill() {
				return nil, -1
			}
			take := min(c.w-c.r, remaining)
			body = append(body, c.buf[c.r:c.r+take]...)
			c.r += take
			remaining -= take
		}
		for c.w-c.r < 2 { // the CRLF closing the chunk
			if !c.fill() {
				return nil, -1
			}
		}
		c.r += 2
	}
}

// handle answers one request; false means the connection must close.
func (c *httpConn) handle(req *httpRequest) (keep bool) {
	head := bytes.Equal(req.method, []byte("HEAD"))
	defer func() {
		if r := recover(); r != nil {
			frame := errorFrame(500, "Server Error: "+jsString(panicValue(r)), nil, "")
			keep = c.write(frame, head)
		}
	}()
	if c.fastIssue(req) || c.fastCreateIssue(req) {
		return true
	}
	if frame := c.fastGit(req); frame != nil {
		return c.write(frame, head)
	}
	return c.write(route(req), head)
}

// lowerKey builds the registry key "owner/name" (ASCII-lowercased) into c.key and reports whether the owner is the
// viewer. Percent-escapes or non-ASCII bytes return false so the general router (which decodes) takes over.
func (c *httpConn) lowerKey(owner, name []byte) bool {
	key := c.key[:0]
	for _, part := range [2][]byte{owner, name} {
		for _, ch := range part {
			if ch >= utf8.RuneSelf || ch == '%' {
				return false
			}
			if ch >= 'A' && ch <= 'Z' {
				ch += 'a' - 'A'
			}
			key = append(key, ch)
		}
		key = append(key, '/')
	}
	c.key = key[:len(key)-1]
	return string(c.key[:len(owner)]) == viewerLower
}

// splitRepoPath splits "{owner}/{repo}/{rest}" (both names non-empty).
func splitRepoPath(path []byte) (owner, name, rest []byte, ok bool) {
	slash1 := bytes.IndexByte(path, '/')
	if slash1 <= 0 {
		return nil, nil, nil, false
	}
	tail := path[slash1+1:]
	slash2 := bytes.IndexByte(tail, '/')
	if slash2 <= 0 {
		return nil, nil, nil, false
	}
	return path[:slash1], tail[:slash2], tail[slash2+1:], true
}

// fastCreateIssue answers POST /api/v3/repos/{owner}/{repo}/issues when the body is the common {title, body?} shape
// (no labels): no generic JSON maps, and the 201 reply goes out in one write. Anything else falls through to the
// general router, which handles every other shape exactly.
func (c *httpConn) fastCreateIssue(req *httpRequest) bool {
	const prefix = "/api/v3/repos/"
	path := req.path
	if string(req.method) != "POST" || len(path) <= len(prefix) || string(path[:len(prefix)]) != prefix || !apiBaseKnown.Load() || !authorized(req.auth) {
		return false
	}
	owner, name, rest, ok := splitRepoPath(path[len(prefix):])
	if !ok || string(rest) != "issues" || !c.lowerKey(owner, name) {
		return false
	}
	title, body, ok := parseIssueBody(req.body)
	if !ok {
		return false
	}
	var f *issueFrame
	var repoOwner, repoName string
	var number int64
	func() {
		storeMu.Lock()
		defer storeMu.Unlock()
		repo := reposByKey[string(c.key)]
		if repo == nil {
			return
		}
		issue := createIssue(repo, title, body, nil, viewerLogin)
		f, repoOwner, repoName, number = issueFrameOf(issue), repo.Owner, repo.Name, issue.Number
	}()
	if f == nil {
		return false
	}
	out := append(c.out[:0], "HTTP/1.1 201 Created\r\nContent-Type: "+jsonContentType+"\r\nContent-Length: "...)
	out = strconv.AppendInt(out, int64(len(f.json)), 10)
	out = append(out, "\r\nX-GitHub-Request-Id: "...)
	out = appendRequestID(out, requestCounter.Add(1))
	out = append(out, "\r\nLocation: "...)
	out = append(out, base()...)
	out = append(out, "/api/v3/repos/"...)
	out = append(out, repoOwner...)
	out = append(out, '/')
	out = append(out, repoName...)
	out = append(out, "/issues/"...)
	out = strconv.AppendInt(out, number, 10)
	out = append(out, "\r\n\r\n"...)
	out = append(out, f.json...)
	c.out = out
	c.nc.Write(out)
	return true
}

// parseIssueBody reads {"title": "...", "body": ...} with exact (case-sensitive) keys and JSON.parse semantics
// (the last duplicate wins, a non-string body is null). It declines (ok false) on a labels key, a title that is not
// a non-blank string, or anything malformed.
func parseIssueBody(raw []byte) (title string, body any, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", nil, false
	}
	hasTitle := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", nil, false
		}
		key, _ := keyTok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return "", nil, false
		}
		switch key {
		case "title":
			var s string
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &s) != nil {
				return "", nil, false
			}
			title, hasTitle = s, true
		case "body":
			body = nil
			if len(value) > 0 && value[0] == '"' {
				var s string
				if json.Unmarshal(value, &s) != nil {
					return "", nil, false
				}
				body = s
			}
		case "labels":
			return "", nil, false
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return "", nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", nil, false
	}
	if !hasTitle || jsTrim(title) == "" {
		return "", nil, false
	}
	return title, body, true
}

// fastGit routes /{owner}/{repo}.git/... straight from the request bytes, with no lowercased copies or key
// strings on the clone/push hot path. Unusual paths (escapes, unknown repositories) go to the general router.
func (c *httpConn) fastGit(req *httpRequest) []byte {
	path := req.path
	if len(path) < 2 || path[0] != '/' || bytes.HasPrefix(path, []byte("/api/v3")) {
		return nil
	}
	slash := bytes.IndexByte(path[1:], '/')
	if slash <= 0 {
		return nil
	}
	owner := path[1 : 1+slash]
	segment, rest := path[2+slash:], []byte(nil)
	if next := bytes.IndexByte(segment, '/'); next >= 0 {
		segment, rest = segment[:next], segment[next:]
	}
	if len(segment) <= 4 || string(segment[len(segment)-4:]) != ".git" || !c.lowerKey(owner, segment[:len(segment)-4]) {
		return nil
	}
	storeMu.RLock()
	repo := reposByKey[string(c.key)]
	storeMu.RUnlock()
	if repo == nil {
		return nil
	}
	setBase(req.host)
	return HandleGit(&repo.GitRepo, &GitRequest{
		Method:          string(req.method),
		Path:            string(rest),
		Query:           string(req.query),
		GitProtocol:     string(req.gitProtocol),
		ContentEncoding: string(req.contentEncoding),
		Body:            req.body,
	})
}

func panicValue(r any) any {
	if e, ok := r.(error); ok {
		return e
	}
	return r
}

func (c *httpConn) write(frame []byte, headOnly bool) bool {
	if headOnly {
		if i := bytes.Index(frame, []byte("\r\n\r\n")); i >= 0 {
			frame = frame[:i+4]
		}
	}
	_, err := c.nc.Write(frame)
	return err == nil
}

// fastIssue answers GET /api/v3/repos/{owner}/{repo}/issues/{number} for an existing issue straight from its
// pre-rendered reply, allocation-free; anything unusual falls through to the general router.
func (c *httpConn) fastIssue(req *httpRequest) bool {
	const prefix = "/api/v3/repos/"
	path := req.path
	if len(path) <= len(prefix) || string(req.method) != "GET" || string(path[:len(prefix)]) != prefix || !apiBaseKnown.Load() || !authorized(req.auth) {
		return false
	}
	owner, name, rest, ok := splitRepoPath(path[len(prefix):])
	if !ok || len(rest) < 8 || string(rest[:7]) != "issues/" {
		return false
	}
	digits := rest[7:]
	if len(digits) == 0 || len(digits) > 18 {
		return false
	}
	var number int64
	for _, d := range digits {
		if d < '0' || d > '9' {
			return false
		}
		number = number*10 + int64(d-'0')
	}
	if !c.lowerKey(owner, name) {
		return false
	}
	storeMu.RLock()
	repo := reposByKey[string(c.key)]
	var f *issueFrame
	if repo != nil {
		if issue := repo.issues[number]; issue != nil {
			f = issueFrameOf(issue)
		}
	}
	storeMu.RUnlock()
	if f == nil {
		return false
	}
	c.nc.Write(f.http) // the pre-rendered reply, request id and all: no copy, no allocation
	return true
}

// ---------------------------------------------------------------- routing (app.ts)

const wwwAuthenticate = "WWW-Authenticate: Bearer realm=\"forge\"\r\n"

var mcpUnauthorized = O("jsonrpc", "2.0", "id", nil, "error", O("code", -32001, "message", "Unauthorized: a bearer token is required"))

func route(req *httpRequest) []byte {
	path := string(req.path)
	search := string(req.query)
	method := string(req.method)
	if strings.HasPrefix(path, "/api/v3") {
		if !authorized(req.auth) {
			return errorFrame(401, "Requires authentication", nil, wwwAuthenticate)
		}
		setBase(req.host)
		return handleREST(&restReq{method: method, path: path, search: search, host: string(req.host), hasHost: req.hasHost, body: req.body})
	}
	if path == "/healthz" {
		return jsonValueFrame(200, O("ok", true), "")
	}
	if path == "/api/graphql" {
		if !authorized(req.auth) {
			return errorFrame(401, "Requires authentication", nil, wwwAuthenticate)
		}
		setBase(req.host)
		if method != "POST" {
			return errorFrame(404, "Not Found", nil, "")
		}
		return handleGraphQL(req.body)
	}
	if path == "/mcp" {
		if !authorized(req.auth) {
			return jsonValueFrame(401, mcpUnauthorized, wwwAuthenticate)
		}
		setBase(req.host)
		switch method {
		case "POST":
			return handleMCP(req.body, string(req.accept))
		case "DELETE":
			return HTTPFrame(204, "", "", nil)
		}
		return jsonValueFrame(405, O("jsonrpc", "2.0", "id", nil, "error", O("code", -32601, "message", "Method not allowed")), "Allow: POST, DELETE\r\n")
	}
	if owner, name, rest, ok := splitGitPath(path); ok {
		owner = jsDecode(owner)
		var repo *Repo
		if strings.ToLower(owner) == viewerLower {
			storeMu.RLock()
			repo = getRepo(owner, jsDecode(name))
			storeMu.RUnlock()
		}
		if repo == nil {
			return HTTPFrame(404, ctPlainText, "Cache-Control: no-cache\r\n", []byte("Repository not found\n"))
		}
		setBase(req.host)
		return HandleGit(&repo.GitRepo, &GitRequest{
			Method:          method,
			Path:            rest,
			Query:           search,
			GitProtocol:     string(req.gitProtocol),
			ContentEncoding: string(req.contentEncoding),
			Body:            req.body,
		})
	}
	return errorFrame(404, "Not Found", nil, "")
}

// splitGitPath matches /^\/([^/]+)\/([^/]+?)\.git(\/.*)?$/.
func splitGitPath(path string) (owner, name, rest string, ok bool) {
	if len(path) < 2 || path[0] != '/' {
		return "", "", "", false
	}
	slash := strings.IndexByte(path[1:], '/')
	if slash <= 0 {
		return "", "", "", false
	}
	owner = path[1 : 1+slash]
	tail := path[2+slash:]
	segment := tail
	if next := strings.IndexByte(tail, '/'); next >= 0 {
		segment, rest = tail[:next], tail[next:]
	}
	if len(segment) <= 4 || !strings.HasSuffix(segment, ".git") {
		return "", "", "", false
	}
	return owner, segment[:len(segment)-4], rest, true
}
