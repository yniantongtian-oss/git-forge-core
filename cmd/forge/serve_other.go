//go:build !linux

package main

import (
	"errors"
	"net"
	"time"
)

func startServer() (int, func(), error) {
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return 0, nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
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
			go serveConn(nc)
		}
	}()
	return port, func() { _ = ln.Close() }, nil
}
