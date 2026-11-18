package main

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"
)

// recordConn is a transport that keeps every reply byte.
type recordConn struct{ out bytes.Buffer }

func (r *recordConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (r *recordConn) Write(p []byte) (int, error) { return r.out.Write(p) }
func (r *recordConn) Close() error                { return nil }

func newTestConn() (*httpConn, *recordConn) {
	rc := &recordConn{}
	return acquireConn(rc), rc
}

// feed appends input the way a transport does: make room, copy, advance w.
func feed(c *httpConn, data []byte) {
	for len(data) > 0 {
		c.room()
		n := copy(c.buf[c.w:], data)
		c.w += n
		data = data[n:]
	}
}

// parseBytewise feeds raw one byte at a time and expects parseMore until the very last byte completes a request.
func parseBytewise(t *testing.T, raw string) (*httpConn, *recordConn, httpRequest) {
	t.Helper()
	c, rc := newTestConn()
	var req httpRequest
	for i := 0; i < len(raw); i++ {
		feed(c, []byte{raw[i]})
		req = httpRequest{}
		status := c.parseNext(&req)
		if i < len(raw)-1 {
			if status != parseMore {
				t.Fatalf("byte %d of %d: status %d, want parseMore", i+1, len(raw), status)
			}
			continue
		}
		if status != parseDone {
			t.Fatalf("last byte: status %d, want parseDone", status)
		}
	}
	return c, rc, req
}

func TestParseSplitHeaders(t *testing.T) {
	c, _, req := parseBytewise(t, "GET /api/v3/user?x=1 HTTP/1.1\r\nHost: forge:80\r\nAuthorization: Bearer t\r\nConnection: keep-alive\r\n\r\n")
	if string(req.method) != "GET" || string(req.path) != "/api/v3/user" || string(req.query) != "x=1" {
		t.Fatalf("request line: %q %q %q", req.method, req.path, req.query)
	}
	if string(req.host) != "forge:80" || string(req.auth) != "Bearer t" || !req.keepAlive || req.body != nil {
		t.Fatalf("headers: host %q auth %q keepAlive %v body %q", req.host, req.auth, req.keepAlive, req.body)
	}
	if c.r != c.w {
		t.Fatalf("request not consumed: r=%d w=%d", c.r, c.w)
	}
}

func TestParseContentLengthBytewise(t *testing.T) {
	_, rc, req := parseBytewise(t, "POST /x HTTP/1.1\r\nContent-Length: 11\r\n\r\nhello world")
	if string(req.body) != "hello world" {
		t.Fatalf("body %q", req.body)
	}
	if rc.out.Len() != 0 {
		t.Fatalf("unexpected output %q", rc.out.String())
	}
}

func TestParseChunkedBytewise(t *testing.T) {
	raw := "POST /git-receive-pack HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"5;ext=1\r\nhello\r\n" + "1\r\n \r\n" + "B \r\nchunked bod\r\n" + "1\r\ny\r\n" + "0\r\nX-Trailer: v\r\n\r\n"
	c, _, req := parseBytewise(t, raw)
	if string(req.body) != "hello chunked body" {
		t.Fatalf("body %q", req.body)
	}
	if c.need != 0 || c.chunkAt != 0 || c.chunkTotal != 0 {
		t.Fatalf("pending state not reset: %+v", c)
	}
}

func TestParseChunkedEmpty(t *testing.T) {
	_, _, req := parseBytewise(t, "POST /x HTTP/1.1\r\ntransfer-encoding: CHUNKED\r\n\r\n0\r\n\r\n")
	if req.body == nil || len(req.body) != 0 {
		t.Fatalf("empty chunked body should be non-nil and empty: %v", req.body)
	}
}

func TestParseExpectContinueOnce(t *testing.T) {
	c, rc := newTestConn()
	feed(c, []byte("POST /x HTTP/1.1\r\nContent-Length: 6\r\nExpect: 100-continue\r\n\r\n"))
	var req httpRequest
	if status := c.parseNext(&req); status != parseMore {
		t.Fatalf("status %d, want parseMore", status)
	}
	feed(c, []byte("abc"))
	if status := c.parseNext(&req); status != parseMore {
		t.Fatalf("status %d, want parseMore", status)
	}
	if got := rc.out.String(); got != "HTTP/1.1 100 Continue\r\n\r\n" {
		t.Fatalf("interim replies %q, want exactly one 100 Continue", got)
	}
	feed(c, []byte("def"))
	req = httpRequest{}
	if status := c.parseNext(&req); status != parseDone || string(req.body) != "abcdef" {
		t.Fatalf("status %d body %q", status, req.body)
	}
	// A body that is already complete gets no interim reply.
	c2, rc2 := newTestConn()
	feed(c2, []byte("POST /x HTTP/1.1\r\nContent-Length: 2\r\nExpect: 100-continue\r\n\r\nok"))
	if status := c2.parseNext(&req); status != parseDone || rc2.out.Len() != 0 {
		t.Fatalf("status %d, output %q", status, rc2.out.String())
	}
}

func TestParseLargeBodyGrowsBuffer(t *testing.T) {
	body := strings.Repeat("0123456789abcdef", 8192) // 128 KiB, past the 16 KiB buffer
	c, _ := newTestConn()
	feed(c, []byte("PUT /big HTTP/1.1\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"))
	var req httpRequest
	for i := 0; i < len(body); i += 1000 {
		if status := c.parseNext(&req); status != parseMore {
			t.Fatalf("at %d: status %d", i, status)
		}
		feed(c, []byte(body[i:min(len(body), i+1000)]))
	}
	req = httpRequest{}
	if status := c.parseNext(&req); status != parseDone || string(req.body) != body || string(req.path) != "/big" {
		t.Fatalf("status %d, body ok %v, path %q", status, string(req.body) == body, req.path)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]int{
		"POST /x HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n":     400,
		"POST /x HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabcXY": 400,
		"POST /x HTTP/1.1\r\nContent-Length: 12a\r\n\r\n":                  400,
		"BROKEN\r\n\r\n": 400,
		"POST /x HTTP/1.1\r\nContent-Length: 999999999999\r\n\r\n": 413,
	}
	for raw, want := range cases {
		c, _ := newTestConn()
		feed(c, []byte(raw))
		var req httpRequest
		if status := c.parseNext(&req); status != want {
			t.Errorf("%q: status %d, want %d", raw, status, want)
		}
	}
	c, _ := newTestConn()
	feed(c, []byte("GET / HTTP/1.1\r\nX-Big: "+strings.Repeat("a", maxHeaderBytes)))
	var req httpRequest
	if status := c.parseNext(&req); status != 431 {
		t.Errorf("oversized head: status %d, want 431", status)
	}
}

func TestProcessPipelinedAndStrayCRLF(t *testing.T) {
	c, rc := newTestConn()
	one := "GET /healthz HTTP/1.1\r\nHost: h\r\n\r\n"
	feed(c, []byte(one+"\r\n"+one+"GET /heal"))
	if result := c.process(); result != procMore {
		t.Fatalf("process %d, want procMore", result)
	}
	if n := strings.Count(rc.out.String(), "HTTP/1.1 200 OK"); n != 2 {
		t.Fatalf("%d replies, want 2:\n%s", n, rc.out.String())
	}
	feed(c, []byte("thz HTTP/1.1\r\nConnection: close\r\n\r\n"))
	if result := c.process(); result != procClose {
		t.Fatalf("process %d, want procClose", result)
	}
	if n := strings.Count(rc.out.String(), `{"ok":true}`); n != 3 {
		t.Fatalf("%d bodies, want 3", n)
	}
}

func TestProcessBadRequestCloses(t *testing.T) {
	c, rc := newTestConn()
	feed(c, []byte("POST /x HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\nnope\r\n"))
	if result := c.process(); result != procClose {
		t.Fatalf("process %d, want procClose", result)
	}
	if !strings.HasPrefix(rc.out.String(), "HTTP/1.1 400 Bad Request") || !strings.Contains(rc.out.String(), "Connection: close") {
		t.Fatalf("reply %q", rc.out.String())
	}
}
