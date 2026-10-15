//go:build linux

package main

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	kindAccept = 1
	kindRecv   = 2
	kindSend   = 3

	bufGroup   = 0
	recvBufs   = 4096 // provided recv buffers shared by all connections
	recvBufSz  = 4096
	maxInBuf   = 64 << 10
	maxOutPool = 256
)

func ud(kind uint64, fd int) uint64 { return kind<<56 | uint64(uint32(fd)) }

type conn struct {
	fd       int
	in       []byte // unparsed bytes carried over between recvs
	out      []byte // bytes in flight to the kernel
	sent     int
	pend     []byte // responses produced while out is in flight
	sending  bool
	recving  bool
	closing  bool // close once nothing is in flight
	queued   bool // already on the flush list
	closeAft bool // "Connection: close" seen
	closed   bool
	waiting  bool // a write from this conn is in the pending batch; parsing paused
}

type server struct {
	r      *ring
	br     *bufRing
	lfd    int
	app    *app
	conns  []*conn
	flush  []*conn
	outBuf [][]byte // free list of response buffers

	// Group commit: writes parsed during one loop pass share one transaction.
	ops      []writeOp
	opConns  []*conn
	batchMax int
}

func listenTCP(host, port string) (int, error) {
	ip := net.ParseIP(host)
	if host == "" || ip == nil {
		ip = net.IPv4zero
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return -1, fmt.Errorf("only IPv4 HOST supported, got %q", host)
	}
	var p int
	if _, err := fmt.Sscan(port, &p); err != nil {
		return -1, fmt.Errorf("bad PORT %q", port)
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	sa := &syscall.SockaddrInet4{Port: p}
	copy(sa.Addr[:], ip4)
	if err := syscall.Bind(fd, sa); err != nil {
		return -1, err
	}
	if err := syscall.Listen(fd, 65535); err != nil {
		return -1, err
	}
	return fd, nil
}

// runURing serves forever on the calling goroutine, which it pins to one OS thread:
// the ring is SINGLE_ISSUER|DEFER_TASKRUN, so every submit must come from that thread,
// and the SQLite connection is opened NOMUTEX for the same reason.
func runURing(lfd int, a *app, batchMax int) error {
	runtime.LockOSThread()
	r, err := newRing(4096, 32768)
	if err != nil {
		return err
	}
	br, err := r.registerBufRing(bufGroup, recvBufs, recvBufSz)
	if err != nil {
		return err
	}
	s := &server{r: r, br: br, lfd: lfd, app: a, conns: make([]*conn, 1024), batchMax: batchMax}
	r.prepAcceptMultishot(lfd, ud(kindAccept, lfd))
	log.Printf("io_uring loop up")
	for {
		r.enter(1, 0)
		t0 := time.Now()
		s.reap()
		t1 := time.Now()
		nops := len(s.ops)
		s.runBatch()
		t2 := time.Now()
		s.flushSends()
		// A pass this long stalls every connection; log what it was doing.
		if d := time.Since(t0); d > 50*time.Millisecond {
			log.Printf("stall: pass=%v reap=%v batch=%v (%d writes) flush=%v",
				d.Round(time.Millisecond), t1.Sub(t0).Round(time.Millisecond), t2.Sub(t1).Round(time.Millisecond), nops, time.Since(t2).Round(time.Millisecond))
		}
	}
}

// passChunk bounds how many completions are handled before queued writes are
// committed and responses are sent (PASS_CHUNK; 0 = once per pass).
var passChunk = func() int {
	if v, ok := parseID([]byte(env("PASS_CHUNK", "0"))); ok {
		return int(v)
	}
	return 0
}()

func (s *server) reap() {
	r := s.r
	n := 0
	for {
		head := *r.cqHead
		tail := atomic.LoadUint32(r.cqTail)
		if head == tail {
			break
		}
		for ; head != tail; head++ {
			s.complete(r.cqes[head&r.cqMask])
			if n++; passChunk > 0 && n%passChunk == 0 {
				// Under a burst, answer what's done so far instead of holding every
				// response until the whole pass is processed.
				atomic.StoreUint32(r.cqHead, head+1)
				s.br.commit()
				s.runBatch()
				s.flushSends()
				r.enter(0, 0)
			}
		}
		atomic.StoreUint32(r.cqHead, head)
	}
	s.br.commit()
}

func (s *server) complete(c cqe) {
	fd := int(uint32(c.userData))
	switch c.userData >> 56 {
	case kindAccept:
		if c.flags&cqeFMore == 0 {
			s.r.prepAcceptMultishot(s.lfd, ud(kindAccept, s.lfd))
		}
		if c.res < 0 {
			return
		}
		nfd := int(c.res)
		syscall.SetsockoptInt(nfd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		for nfd >= len(s.conns) {
			s.conns = append(s.conns, make([]*conn, len(s.conns))...)
		}
		cn := &conn{fd: nfd, recving: true}
		s.conns[nfd] = cn
		s.r.prepRecvMultishot(nfd, bufGroup, ud(kindRecv, nfd))

	case kindRecv:
		cn := s.conns[fd]
		more := c.flags&cqeFMore != 0
		if c.flags&cqeFBuffer != 0 {
			bid := uint16(c.flags >> 16)
			if c.res > 0 && !cn.closing {
				s.onData(cn, s.br.buf(bid)[:c.res])
			}
			s.br.add(bid)
		}
		if more {
			return
		}
		cn.recving = false
		switch {
		case c.res == -int32(syscall.ENOBUFS) && !cn.closing:
			// Ran out of provided buffers; give them back before re-arming.
			s.br.commit()
			fallthrough
		case c.res > 0 && !cn.closing:
			cn.recving = true
			s.r.prepRecvMultishot(fd, bufGroup, ud(kindRecv, fd))
		default: // EOF or error
			cn.closing = true
			s.maybeClose(cn)
		}

	case kindSend:
		cn := s.conns[fd]
		if c.res < 0 {
			cn.sending = false
			s.abort(cn)
			return
		}
		cn.sent += int(c.res)
		if cn.sent < len(cn.out) {
			s.r.prepSend(fd, cn.out[cn.sent:], ud(kindSend, fd))
			return
		}
		cn.sending = false
		cn.out, cn.pend = cn.pend, s.putBuf(cn.out)
		cn.sent = 0
		if len(cn.out) > 0 {
			s.startSend(cn)
		} else {
			cn.out = s.putBuf(cn.out)
			if cn.closeAft {
				s.abort(cn)
			}
		}
		s.maybeClose(cn)
	}
}

// abort shuts the socket down so the outstanding multishot recv terminates; the
// fd itself is closed once nothing references it.
func (s *server) abort(cn *conn) {
	if !cn.closing {
		cn.closing = true
		syscall.Shutdown(cn.fd, syscall.SHUT_RDWR)
	}
	s.maybeClose(cn)
}

func (s *server) maybeClose(cn *conn) {
	if cn.closing && !cn.sending && !cn.recving && !cn.closed {
		cn.closed = true
		syscall.Close(cn.fd)
		if s.conns[cn.fd] == cn {
			s.conns[cn.fd] = nil
		}
		s.putBuf(cn.out)
		s.putBuf(cn.pend)
	}
}

func (s *server) getBuf() []byte {
	if n := len(s.outBuf); n > 0 {
		b := s.outBuf[n-1]
		s.outBuf = s.outBuf[:n-1]
		return b
	}
	return make([]byte, 0, 8<<10)
}

// putBuf recycles b and returns nil so callers can write `x = s.putBuf(x)`.
func (s *server) putBuf(b []byte) []byte {
	if b != nil && cap(b) <= 64<<10 && len(s.outBuf) < maxOutPool {
		s.outBuf = append(s.outBuf, b[:0])
	}
	return nil
}

// onData parses as many complete requests as possible from data and queues responses.
func (s *server) onData(cn *conn, data []byte) {
	if cn.waiting {
		// Responses must stay in order: hold everything behind the pending write.
		if len(cn.in)+len(data) > maxInBuf {
			s.abort(cn)
			return
		}
		cn.in = append(cn.in, data...)
		return
	}
	if len(cn.in) > 0 {
		cn.in = append(cn.in, data...)
		data = cn.in
	}
	n := s.parse(cn, data)
	if n < 0 {
		s.abort(cn)
		return
	}
	rest := data[n:]
	if len(rest) > maxInBuf {
		s.abort(cn)
		return
	}
	if len(cn.in) > 0 {
		cn.in = cn.in[:copy(cn.in, rest)]
	} else if len(rest) > 0 {
		cn.in = append(make([]byte, 0, len(rest)+recvBufSz), rest...)
	}
	if len(cn.in) == 0 {
		cn.in = nil
	}
	if !cn.queued && (len(cn.pend) > 0) {
		cn.queued = true
		s.flush = append(s.flush, cn)
	}
}

var crlf = []byte("\r\n")

// parse handles every complete request in data, appending responses to cn.pend.
// Returns bytes consumed, or -1 on a malformed request.
func (s *server) parse(cn *conn, data []byte) int {
	consumed := 0
	for !cn.closeAft {
		buf := data[consumed:]
		end := bytes.Index(buf, []byte("\r\n\r\n"))
		if end < 0 {
			return consumed
		}
		head := buf[:end]
		lineEnd := bytes.Index(head, crlf)
		if lineEnd < 0 {
			lineEnd = len(head)
		}
		line := head[:lineEnd]
		sp1 := bytes.IndexByte(line, ' ')
		if sp1 < 0 {
			return -1
		}
		sp2 := bytes.IndexByte(line[sp1+1:], ' ')
		if sp2 < 0 {
			return -1
		}
		method, path, proto := line[:sp1], line[sp1+1:sp1+1+sp2], line[sp1+2+sp2:]
		closeAft := string(proto) == "HTTP/1.0"

		var auth []byte
		clen := 0
		hdrs := head[lineEnd:]
		for len(hdrs) > 0 {
			if bytes.HasPrefix(hdrs, crlf) {
				hdrs = hdrs[2:]
			}
			e := bytes.Index(hdrs, crlf)
			var h []byte
			if e < 0 {
				h, hdrs = hdrs, nil
			} else {
				h, hdrs = hdrs[:e], hdrs[e:]
			}
			colon := bytes.IndexByte(h, ':')
			if colon < 0 {
				continue
			}
			name, val := h[:colon], bytes.TrimSpace(h[colon+1:])
			switch {
			case len(name) == 14 && asciiEqualFold(name, "content-length"):
				n, ok := parseID(val)
				if !ok && string(val) != "0" {
					return -1
				}
				clen = int(n)
			case len(name) == 13 && asciiEqualFold(name, "authorization"):
				auth = val
			case len(name) == 10 && asciiEqualFold(name, "connection"):
				if asciiEqualFold(val, "close") {
					closeAft = true
				} else if asciiEqualFold(val, "keep-alive") {
					closeAft = false
				}
			}
		}
		total := end + 4 + clen
		if len(buf) < total {
			return consumed
		}
		if cn.pend == nil {
			cn.pend = s.getBuf()
		}
		cn.closeAft = closeAft
		consumed += total
		if string(method) == "POST" {
			op, isWrite, st, eb := s.app.prepareWrite(path, auth, buf[end+4:total])
			if isWrite {
				if eb != nil {
					cn.pend = appendResponse(cn.pend, st, eb)
					continue
				}
				s.ops = append(s.ops, op)
				s.opConns = append(s.opConns, cn)
				cn.waiting = true
				return consumed
			}
		}
		cn.pend = s.app.handle(cn.pend, method, path, auth, buf[end+4:total])
	}
	return consumed
}

func asciiEqualFold(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != s[i] {
			return false
		}
	}
	return true
}

func (s *server) flushSends() {
	for _, cn := range s.flush {
		cn.queued = false
		if cn.sending || cn.closing || len(cn.pend) == 0 {
			continue
		}
		cn.out, cn.pend = cn.pend, cn.out
		cn.sent = 0
		s.startSend(cn)
	}
	clear(s.flush)
	s.flush = s.flush[:0]
}

func (s *server) startSend(cn *conn) {
	cn.sending = true
	s.r.prepSend(cn.fd, cn.out, ud(kindSend, cn.fd))
}

// runBatch commits the writes queued this loop pass, batchMax per transaction,
// then answers them and resumes parsing each connection's held-back input.
func (s *server) runBatch() {
	for len(s.ops) > 0 {
		ops, conns := s.ops, s.opConns
		s.ops, s.opConns = nil, nil
		for i := 0; i < len(ops); i += s.batchMax {
			j := min(i+s.batchMax, len(ops))
			s.app.execBatch(ops[i:j])
		}
		for i := range ops {
			cn := conns[i]
			cn.waiting = false
			if cn.closing {
				continue
			}
			st, b := s.app.renderWrite(&ops[i])
			if cn.pend == nil {
				cn.pend = s.getBuf()
			}
			cn.pend = appendResponse(cn.pend, st, b)
			// Resume pipelined input; it may queue another write (next round).
			in := cn.in
			cn.in = nil
			s.onData(cn, in)
		}
		// Reuse the slices' backing arrays next pass unless resumed input queued more.
		if len(s.ops) == 0 {
			clear(ops)
			clear(conns)
			s.ops, s.opConns = ops[:0], conns[:0]
		}
	}
}
