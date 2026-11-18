//go:build linux

package main

// The Linux transport: one goroutine, pinned to its OS thread, runs a non-blocking epoll reactor over the
// listener and every client socket.
//
// Why a single reactor thread wins in a 2-vCPU gVisor pod: the forge's own work per request is microseconds (hot
// replies are pre-rendered) while the co-located Node benchmark client and the gVisor Sentry keep both CPUs busy.
// The reactor spawns no extra OS threads, never parks a goroutine or wakes another thread per request, reads a
// socket only after epoll reported data (no speculative read -> EAGAIN), and takes up to 128 ready sockets per
// epoll_wait during 50-way concurrent clones. Requests go through the same incremental parser as the portable
// transport (httpConn.process); a reply the socket cannot take at once is queued and flushed on EPOLLOUT.

import (
	"io"
	"runtime"
	"syscall"
)

// startServer serves 0.0.0.0:0 from the epoll reactor, falling back to the netpoller transport if it cannot start.
func startServer() (int, func(), error) {
	r, port, err := newReactor()
	if err != nil {
		return startNetServer()
	}
	go r.run()
	return port, r.stop, nil
}

type reactor struct {
	lfd       int
	epfd      int
	wake      [2]int         // a pipe: stop writes wake[1]; the loop sees wake[0] readable and stops
	conns     []*reactorConn // indexed by file descriptor
	acceptOff bool           // accepting paused (EMFILE and friends) until a connection closes
	events    [128]syscall.EpollEvent
	ev        syscall.EpollEvent // the EpollCtl argument, reused
}

// reactorConn is the transport side of one client socket. Handlers write replies to it (as httpConn.nc).
type reactorConn struct {
	fd      int
	hc      *httpConn
	pending []byte // reply bytes the socket has not taken yet
	closing bool   // close once pending drains
	dead    bool   // a write failed: close at the next opportunity
}

func newReactor() (*reactor, int, error) {
	r := &reactor{lfd: -1, epfd: -1, wake: [2]int{-1, -1}}
	fail := func(err error) (*reactor, int, error) {
		for _, fd := range []int{r.lfd, r.epfd, r.wake[0], r.wake[1]} {
			if fd >= 0 {
				_ = syscall.Close(fd)
			}
		}
		return nil, 0, err
	}
	var err error
	if r.lfd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0); err != nil {
		return fail(err)
	}
	_ = syscall.SetsockoptInt(r.lfd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	if err = syscall.Bind(r.lfd, &syscall.SockaddrInet4{}); err != nil {
		return fail(err)
	}
	if err = syscall.Listen(r.lfd, 4096); err != nil {
		return fail(err)
	}
	sa, err := syscall.Getsockname(r.lfd)
	if err != nil {
		return fail(err)
	}
	inet, ok := sa.(*syscall.SockaddrInet4)
	if !ok {
		return fail(syscall.EAFNOSUPPORT)
	}
	if r.epfd, err = syscall.EpollCreate1(syscall.EPOLL_CLOEXEC); err != nil {
		return fail(err)
	}
	if err = syscall.Pipe2(r.wake[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		return fail(err)
	}
	if err = r.ctl(syscall.EPOLL_CTL_ADD, r.lfd, syscall.EPOLLIN); err != nil {
		return fail(err)
	}
	if err = r.ctl(syscall.EPOLL_CTL_ADD, r.wake[0], syscall.EPOLLIN); err != nil {
		return fail(err)
	}
	return r, inet.Port, nil
}

func (r *reactor) ctl(op, fd int, events uint32) error {
	r.ev = syscall.EpollEvent{Events: events, Fd: int32(fd)}
	return syscall.EpollCtl(r.epfd, op, fd, &r.ev)
}

// stop asks the reactor to close the listener; safe from any goroutine.
func (r *reactor) stop() {
	_, _ = syscall.Write(r.wake[1], []byte{1})
}

func (r *reactor) run() {
	runtime.LockOSThread()
	for {
		n, err := syscall.EpollWait(r.epfd, r.events[:], -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		for i := 0; i < n; i++ {
			fd, events := int(r.events[i].Fd), r.events[i].Events
			switch fd {
			case r.lfd:
				r.accept()
			case r.wake[0]:
				_ = syscall.Close(r.lfd)
				return
			default:
				r.service(fd, events)
			}
		}
	}
}

// accept takes every pending connection (the listener is level-triggered, so EAGAIN means drained).
func (r *reactor) accept() {
	for {
		fd, _, err := syscall.Accept4(r.lfd, syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC)
		if err != nil {
			switch err {
			case syscall.EINTR, syscall.ECONNABORTED:
				continue
			case syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM:
				// Out of descriptors: stop watching the listener until a connection closes, or epoll would spin.
				r.acceptOff = true
				_ = r.ctl(syscall.EPOLL_CTL_MOD, r.lfd, 0)
			}
			return
		}
		_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		if err := r.ctl(syscall.EPOLL_CTL_ADD, fd, syscall.EPOLLIN); err != nil {
			_ = syscall.Close(fd)
			continue
		}
		rc := &reactorConn{fd: fd}
		rc.hc = acquireConn(rc)
		for fd >= len(r.conns) {
			r.conns = append(r.conns, make([]*reactorConn, len(r.conns)+64)...)
		}
		r.conns[fd] = rc
	}
}

// service handles one epoll event: flush a queued reply on EPOLLOUT, or read what arrived and answer it.
func (r *reactor) service(fd int, events uint32) {
	if fd < 0 || fd >= len(r.conns) || r.conns[fd] == nil {
		return
	}
	rc := r.conns[fd]
	if len(rc.pending) > 0 {
		if events&(syscall.EPOLLOUT|syscall.EPOLLERR|syscall.EPOLLHUP) == 0 {
			return
		}
		if !rc.flush() {
			r.close(rc)
			return
		}
		if len(rc.pending) > 0 {
			return // the socket is full again: wait for the next EPOLLOUT
		}
		rc.hc.stalled = false
		if rc.closing {
			r.close(rc)
			return
		}
		if err := r.ctl(syscall.EPOLL_CTL_MOD, fd, syscall.EPOLLIN); err != nil {
			r.close(rc)
			return
		}
		r.drive(rc) // answer pipelined requests that arrived while the reply was flushing
		return
	}
	if events&(syscall.EPOLLIN|syscall.EPOLLERR|syscall.EPOLLHUP) == 0 {
		return
	}
	c := rc.hc
	c.room()
	n, err := syscall.Read(fd, c.buf[c.w:])
	switch {
	case n > 0:
		c.w += n
		r.drive(rc)
	case err == syscall.EAGAIN || err == syscall.EINTR:
		// nothing to read after all; level-triggered epoll reports the socket again when data comes
	default: // end of stream or a read error
		r.close(rc)
	}
}

// drive answers the buffered requests and points epoll at what the connection waits for next.
func (r *reactor) drive(rc *reactorConn) {
	result := rc.hc.process()
	switch {
	case rc.dead:
		r.close(rc)
	case len(rc.pending) > 0:
		rc.closing = result == procClose
		if err := r.ctl(syscall.EPOLL_CTL_MOD, rc.fd, syscall.EPOLLOUT); err != nil {
			r.close(rc)
		}
	case result == procClose:
		r.close(rc)
	}
}

func (r *reactor) close(rc *reactorConn) {
	if rc.fd < 0 || r.conns[rc.fd] != rc {
		return
	}
	r.conns[rc.fd] = nil
	_ = syscall.Close(rc.fd) // closing also removes the descriptor from the epoll set
	rc.fd = -1
	rc.pending = nil
	releaseConn(rc.hc)
	rc.hc = nil
	if r.acceptOff {
		r.acceptOff = false
		_ = r.ctl(syscall.EPOLL_CTL_MOD, r.lfd, syscall.EPOLLIN)
	}
}

// Write sends a reply right away as far as the socket takes it and queues the rest for EPOLLOUT; it never blocks.
func (rc *reactorConn) Write(p []byte) (int, error) {
	if rc.dead {
		return 0, syscall.EPIPE
	}
	if len(rc.pending) > 0 {
		rc.pending = append(rc.pending, p...)
		return len(p), nil
	}
	total := len(p)
	for len(p) > 0 {
		n, err := syscall.Write(rc.fd, p)
		if n > 0 {
			p = p[n:]
		}
		switch {
		case err == syscall.EAGAIN || (err == nil && n <= 0):
			rc.pending = append(make([]byte, 0, len(p)), p...)
			rc.hc.stalled = true
			return total, nil
		case err == syscall.EINTR:
		case err != nil:
			rc.dead = true
			return total - len(p), err
		}
	}
	return total, nil
}

// flush writes queued reply bytes; false when the connection is broken.
func (rc *reactorConn) flush() bool {
	for len(rc.pending) > 0 {
		n, err := syscall.Write(rc.fd, rc.pending)
		if n > 0 {
			rc.pending = rc.pending[n:]
		}
		switch {
		case err == syscall.EAGAIN || (err == nil && n <= 0):
			return true
		case err == syscall.EINTR:
		case err != nil:
			return false
		}
	}
	rc.pending = nil
	return true
}

// Read and Close complete io.ReadWriteCloser; the reactor itself reads and closes the socket.
func (rc *reactorConn) Read([]byte) (int, error) { return 0, io.EOF }
func (rc *reactorConn) Close() error             { return nil }
