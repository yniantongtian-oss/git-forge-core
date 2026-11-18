//go:build linux

package main

import (
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

const maxBlockingConns = 160

var activeBlockingConns atomic.Int32

type linuxConn struct {
	fd int32
}

func (c *linuxConn) Read(p []byte) (int, error) {
	for {
		fd := atomic.LoadInt32(&c.fd)
		if fd < 0 {
			return 0, io.EOF
		}
		n, err := syscall.Read(int(fd), p)
		if err == syscall.EINTR {
			continue
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		return n, err
	}
}

func (c *linuxConn) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		fd := atomic.LoadInt32(&c.fd)
		if fd < 0 {
			return total, net.ErrClosed
		}
		n, err := syscall.Write(int(fd), p)
		if n > 0 {
			total += n
			p = p[n:]
		}
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (c *linuxConn) Close() error {
	fd := atomic.SwapInt32(&c.fd, -1)
	if fd < 0 {
		return nil
	}
	return syscall.Close(int(fd))
}

// startServer binds an ephemeral IPv4 port with a blocking socket so accepted connections use direct blocking
// read/write syscalls without Go netpoller's non-blocking EAGAIN + epoll_pwait overhead under gVisor.
func startServer() (int, func(), error) {
	lfd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return 0, nil, err
	}
	_ = syscall.SetsockoptInt(lfd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	if err := syscall.Bind(lfd, &syscall.SockaddrInet4{Port: 0}); err != nil {
		syscall.Close(lfd)
		return 0, nil, err
	}
	if err := syscall.Listen(lfd, 4096); err != nil {
		syscall.Close(lfd)
		return 0, nil, err
	}
	sa, err := syscall.Getsockname(lfd)
	if err != nil {
		syscall.Close(lfd)
		return 0, nil, err
	}
	port := sa.(*syscall.SockaddrInet4).Port
	var closed atomic.Bool
	stop := func() {
		if closed.CompareAndSwap(false, true) {
			_ = syscall.Shutdown(lfd, syscall.SHUT_RDWR)
			_ = syscall.Close(lfd)
		}
	}
	go acceptLinux(lfd, &closed)
	return port, stop, nil
}

func acceptLinux(lfd int, closed *atomic.Bool) {
	rcvTimeout := syscall.Timeval{Sec: 60, Usec: 0}
	for {
		cfd, _, err := syscall.Accept4(lfd, syscall.SOCK_CLOEXEC)
		if err != nil {
			if closed.Load() || err == syscall.EBADF || err == syscall.EINVAL {
				return
			}
			if err == syscall.EINTR || err == syscall.EAGAIN {
				continue
			}
			time.Sleep(time.Millisecond)
			continue
		}
		_ = syscall.SetsockoptInt(cfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		active := activeBlockingConns.Add(1)
		if active <= maxBlockingConns {
			if active > 80 {
				_ = syscall.SetsockoptTimeval(cfd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &rcvTimeout)
			}
			go func(fd int) {
				defer activeBlockingConns.Add(-1)
				serveConn(&linuxConn{fd: int32(fd)})
			}(cfd)
			continue
		}
		activeBlockingConns.Add(-1)
		f := os.NewFile(uintptr(cfd), "tcp")
		nc, ferr := net.FileConn(f)
		f.Close()
		if ferr != nil {
			continue
		}
		if tc, ok := nc.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		go serveConn(nc)
	}
}
