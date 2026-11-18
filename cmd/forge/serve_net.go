package main

// The portable transport: Go's netpoller with one goroutine per connection. It serves non-Linux hosts (Windows,
// where grade-local runs) and is the Linux fallback when the epoll reactor cannot start. It drives the same
// incremental parser as the reactor (httpConn.process), so local runs exercise exactly the production request
// state machine.

import (
	"errors"
	"net"
	"time"
)

// startNetServer listens on 0.0.0.0:0 and serves every connection on its own goroutine.
func startNetServer() (int, func(), error) {
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return 0, nil, err
	}
	go func() {
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
				_ = tc.SetNoDelay(true)
			}
			go serveNetConn(nc)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = ln.Close() }, nil
}

// serveNetConn alternates blocking reads with process until the connection ends. Writes block until complete, so
// process never stalls here.
func serveNetConn(nc net.Conn) {
	c := acquireConn(nc)
	defer func() {
		_ = nc.Close()
		releaseConn(c)
	}()
	for c.process() != procClose {
		c.room()
		n, err := nc.Read(c.buf[c.w:])
		c.w += n
		if n == 0 && err != nil {
			return
		}
	}
}
