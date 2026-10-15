//go:build linux

// A minimal raw io_uring binding: just enough for multishot accept, multishot recv
// into a provided buffer ring, and send. No liburing, no dependencies.
package main

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	sysIOUringSetup    = 425
	sysIOUringEnter    = 426
	sysIOUringRegister = 427

	setupCQSize       = 1 << 3
	setupCoopTaskrun  = 1 << 8
	setupSingleIssuer = 1 << 12
	setupDeferTaskrun = 1 << 13

	featSingleMmap = 1 << 0

	enterGetEvents = 1 << 0

	offSQRing = 0
	offCQRing = 0x8000000
	offSQEs   = 0x10000000

	opSend   = 26
	opRecv   = 27
	opAccept = 13

	sqeBufferSelect = 1 << 5

	acceptMultishot = 1 << 0
	recvMultishot   = 1 << 1

	cqeFBuffer = 1 << 0
	cqeFMore   = 1 << 1

	registerPbufRing = 22

	msgNoSignal = 0x4000
)

type sqringOffsets struct {
	head, tail, ringMask, ringEntries, flags, dropped, array, resv1 uint32
	userAddr                                                        uint64
}

type cqringOffsets struct {
	head, tail, ringMask, ringEntries, overflow, cqes, flags, resv1 uint32
	userAddr                                                        uint64
}

type uringParams struct {
	sqEntries, cqEntries, flags, sqThreadCPU, sqThreadIdle, features, wqFd uint32
	resv                                                                   [3]uint32
	sqOff                                                                  sqringOffsets
	cqOff                                                                  cqringOffsets
}

type sqe struct {
	opcode      uint8
	flags       uint8
	ioprio      uint16
	fd          int32
	off         uint64
	addr        uint64
	len         uint32
	opFlags     uint32
	userData    uint64
	bufIndex    uint16
	personality uint16
	spliceFdIn  int32
	addr3       uint64
	_           uint64
}

type cqe struct {
	userData uint64
	res      int32
	flags    uint32
}

type ring struct {
	fd int

	sqHead, sqTail *uint32
	sqMask         uint32
	sqes           []sqe
	sqLocalTail    uint32 // tail we've filled up to but not yet published

	cqHead, cqTail *uint32
	cqMask         uint32
	cqes           []cqe

	ringMem, sqeMem []byte
}

func newRing(entries, cqEntries uint32) (*ring, error) {
	p := uringParams{
		flags:     setupCQSize | setupCoopTaskrun | setupSingleIssuer | setupDeferTaskrun,
		cqEntries: cqEntries,
	}
	fd, _, errno := syscall.Syscall(sysIOUringSetup, uintptr(entries), uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}
	if p.features&featSingleMmap == 0 {
		return nil, fmt.Errorf("kernel lacks IORING_FEAT_SINGLE_MMAP")
	}
	r := &ring{fd: int(fd)}

	sz := int(p.sqOff.array + p.sqEntries*4)
	if c := int(p.cqOff.cqes + p.cqEntries*uint32(unsafe.Sizeof(cqe{}))); c > sz {
		sz = c
	}
	mem, err := syscall.Mmap(r.fd, offSQRing, sz, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		return nil, fmt.Errorf("mmap rings: %w", err)
	}
	r.ringMem = mem
	base := unsafe.Pointer(&mem[0])
	r.sqHead = (*uint32)(unsafe.Add(base, p.sqOff.head))
	r.sqTail = (*uint32)(unsafe.Add(base, p.sqOff.tail))
	r.sqMask = *(*uint32)(unsafe.Add(base, p.sqOff.ringMask))
	r.cqHead = (*uint32)(unsafe.Add(base, p.cqOff.head))
	r.cqTail = (*uint32)(unsafe.Add(base, p.cqOff.tail))
	r.cqMask = *(*uint32)(unsafe.Add(base, p.cqOff.ringMask))
	r.cqes = unsafe.Slice((*cqe)(unsafe.Add(base, p.cqOff.cqes)), p.cqEntries)

	// Identity-map the SQ index array once; afterwards slot i is always sqes[i].
	arr := unsafe.Slice((*uint32)(unsafe.Add(base, p.sqOff.array)), p.sqEntries)
	for i := range arr {
		arr[i] = uint32(i)
	}

	sqeSz := int(p.sqEntries) * int(unsafe.Sizeof(sqe{}))
	r.sqeMem, err = syscall.Mmap(r.fd, offSQEs, sqeSz, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		return nil, fmt.Errorf("mmap sqes: %w", err)
	}
	r.sqes = unsafe.Slice((*sqe)(unsafe.Pointer(&r.sqeMem[0])), p.sqEntries)
	r.sqLocalTail = atomic.LoadUint32(r.sqTail)
	return r, nil
}

// getSQE returns a zeroed SQE, flushing the queue to the kernel if it is full.
func (r *ring) getSQE() *sqe {
	for r.sqLocalTail-atomic.LoadUint32(r.sqHead) > r.sqMask {
		r.enter(0, 0)
	}
	s := &r.sqes[r.sqLocalTail&r.sqMask]
	*s = sqe{}
	r.sqLocalTail++
	return s
}

// enter publishes pending SQEs and optionally waits for minComplete CQEs.
func (r *ring) enter(minComplete uint32, flags uintptr) {
	atomic.StoreUint32(r.sqTail, r.sqLocalTail)
	toSubmit := r.sqLocalTail - atomic.LoadUint32(r.sqHead)
	if minComplete > 0 {
		flags |= enterGetEvents
	}
	for {
		_, _, errno := syscall.Syscall6(sysIOUringEnter, uintptr(r.fd), uintptr(toSubmit), uintptr(minComplete), flags, 0, 0)
		switch errno {
		case 0:
			return
		case syscall.EINTR:
			continue
		case syscall.EBUSY, syscall.EAGAIN:
			// CQ backlog: caller must reap completions before more can be submitted.
			return
		default:
			panic(fmt.Sprintf("io_uring_enter: %v", errno))
		}
	}
}

func (r *ring) prepAcceptMultishot(fd int, ud uint64) {
	s := r.getSQE()
	s.opcode = opAccept
	s.fd = int32(fd)
	s.ioprio = acceptMultishot
	s.userData = ud
}

func (r *ring) prepRecvMultishot(fd int, bgid uint16, ud uint64) {
	s := r.getSQE()
	s.opcode = opRecv
	s.fd = int32(fd)
	s.ioprio = recvMultishot
	s.flags = sqeBufferSelect
	s.bufIndex = bgid
	s.userData = ud
}

func (r *ring) prepSend(fd int, b []byte, ud uint64) {
	s := r.getSQE()
	s.opcode = opSend
	s.fd = int32(fd)
	s.addr = uint64(uintptr(unsafe.Pointer(&b[0])))
	s.len = uint32(len(b))
	s.opFlags = msgNoSignal
	s.userData = ud
}

// bufRing is a kernel-registered provided buffer ring (IORING_REGISTER_PBUF_RING):
// the kernel picks a free buffer for each multishot recv completion, so idle
// connections cost no receive memory at all.
type bufRing struct {
	entries []ioUringBuf // shares memory with the kernel; tail lives in entries[0].resv
	tail    *uint16
	mask    uint16
	data    []byte // backing storage for all buffers (mmap'd, outside the Go heap)
	bufSize int
	pending uint16
}

type ioUringBuf struct {
	addr uint64
	len  uint32
	bid  uint16
	resv uint16
}

type bufReg struct {
	ringAddr    uint64
	ringEntries uint32
	bgid        uint16
	flags       uint16
	resv        [3]uint64
}

func (r *ring) registerBufRing(bgid uint16, count, size int) (*bufRing, error) {
	ringMem, err := syscall.Mmap(-1, 0, count*16, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	data, err := syscall.Mmap(-1, 0, count*size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, err
	}
	reg := bufReg{ringAddr: uint64(uintptr(unsafe.Pointer(&ringMem[0]))), ringEntries: uint32(count), bgid: bgid}
	if _, _, errno := syscall.Syscall6(sysIOUringRegister, uintptr(r.fd), registerPbufRing, uintptr(unsafe.Pointer(&reg)), 1, 0, 0); errno != 0 {
		return nil, fmt.Errorf("register pbuf ring: %w", errno)
	}
	br := &bufRing{
		entries: unsafe.Slice((*ioUringBuf)(unsafe.Pointer(&ringMem[0])), count),
		mask:    uint16(count - 1),
		data:    data,
		bufSize: size,
	}
	br.tail = &br.entries[0].resv
	for i := 0; i < count; i++ {
		br.add(uint16(i))
	}
	br.commit()
	return br, nil
}

func (b *bufRing) buf(bid uint16) []byte {
	off := int(bid) * b.bufSize
	return b.data[off : off+b.bufSize]
}

// add queues buffer bid for return to the kernel; commit publishes it.
func (b *bufRing) add(bid uint16) {
	t := *b.tail + b.pending // tail is only written by us
	e := &b.entries[t&b.mask]
	e.addr = uint64(uintptr(unsafe.Pointer(&b.data[int(bid)*b.bufSize])))
	e.len = uint32(b.bufSize)
	e.bid = bid
	b.pending++
}

func (b *bufRing) commit() {
	if b.pending == 0 {
		return
	}
	// The tail is a u16 sharing a word with entries[0]; do a release store on the
	// containing uint32 so the kernel observes the entry writes first.
	word := (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(b.tail)) &^ 3))
	old := atomic.LoadUint32(word)
	newTail := uint32(*b.tail + b.pending)
	// tail is the high half of the word on little-endian (resv sits at byte offset 14).
	atomic.StoreUint32(word, (old&0x0000ffff)|(newTail<<16))
	b.pending = 0
}
