package kmoddp

// A small generic netlink client: one socket, requests matched to replies by sequence
// number, and the kernel's unsolicited messages (sequence 0) handed to a callback.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

var ne = binary.NativeEndian

const (
	genlHdrLen = 4
	nlaHdrLen  = 4

	ctrlCmdGetFamily   = 3
	ctrlAttrFamilyID   = 1
	ctrlAttrFamilyName = 2
)

func align4(n int) int { return (n + 3) &^ 3 }

// attrs builds a netlink attribute list.
type attrs []byte

func (a attrs) add(typ uint16, val []byte) attrs {
	l := nlaHdrLen + len(val)
	b := make([]byte, align4(l))
	ne.PutUint16(b[0:], uint16(l))
	ne.PutUint16(b[2:], typ)
	copy(b[nlaHdrLen:], val)
	return append(a, b...)
}

func (a attrs) u8(typ uint16, v uint8) attrs { return a.add(typ, []byte{v}) }
func (a attrs) u16(typ uint16, v uint16) attrs {
	b := make([]byte, 2)
	ne.PutUint16(b, v)
	return a.add(typ, b)
}
func (a attrs) be16(typ uint16, v uint16) attrs {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return a.add(typ, b)
}
func (a attrs) u32(typ uint16, v uint32) attrs {
	b := make([]byte, 4)
	ne.PutUint32(b, v)
	return a.add(typ, b)
}
func (a attrs) str(typ uint16, s string) attrs { return a.add(typ, append([]byte(s), 0)) }

// parsed is one message's attributes; multi-valued ones (nests repeated) keep every value.
type parsed map[uint16][][]byte

func parseAttrs(b []byte) parsed {
	p := parsed{}
	for len(b) >= nlaHdrLen {
		l := int(ne.Uint16(b[0:]))
		if l < nlaHdrLen || l > len(b) {
			break
		}
		typ := ne.Uint16(b[2:]) & 0x3fff
		p[typ] = append(p[typ], b[nlaHdrLen:l])
		if align4(l) >= len(b) {
			break
		}
		b = b[align4(l):]
	}
	return p
}

func (p parsed) bytes(t uint16) []byte {
	if v := p[t]; len(v) > 0 {
		return v[0]
	}
	return nil
}

func (p parsed) u8(t uint16) uint8 {
	if b := p.bytes(t); len(b) >= 1 {
		return b[0]
	}
	return 0
}

func (p parsed) u16be(t uint16) uint16 {
	if b := p.bytes(t); len(b) >= 2 {
		return binary.BigEndian.Uint16(b)
	}
	return 0
}

func (p parsed) u32(t uint16) uint32 {
	if b := p.bytes(t); len(b) >= 4 {
		return ne.Uint32(b)
	}
	return 0
}

func (p parsed) u64(t uint16) uint64 {
	if b := p.bytes(t); len(b) >= 8 {
		return ne.Uint64(b)
	}
	return 0
}

// msg is one generic netlink message from the kernel.
type msg struct {
	cmd   uint8
	attrs parsed
}

type reply struct {
	msgs []msg
	err  error
}

type conn struct {
	fd       int
	family   uint16
	seq      atomic.Uint32
	mu       sync.Mutex
	pending  map[uint32]chan reply
	partial  map[uint32][]msg
	onEvent  func(msg)
	closed   atomic.Bool
	readDone chan struct{}
	readErr  error
}

// errnoError is a request the kernel refused.
type errnoError struct {
	errno unix.Errno
	msg   string
}

func (e *errnoError) Error() string {
	if e.msg != "" {
		return fmt.Sprintf("%v: %s", e.errno, e.msg)
	}
	return e.errno.Error()
}

func dial(familyName string, onEvent func(msg)) (*conn, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_GENERIC)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	// Punts and liveness events arrive unsolicited: room for bursts of them.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, 8<<20)
	_ = unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_EXT_ACK, 1)
	// The reader wakes up now and then to notice Close.
	tv := unix.Timeval{Usec: 200000}
	_ = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
	c := &conn{
		fd:       fd,
		family:   unix.GENL_ID_CTRL,
		pending:  map[uint32]chan reply{},
		partial:  map[uint32][]msg{},
		onEvent:  onEvent,
		readDone: make(chan struct{}),
	}
	go c.read()
	r, err := c.do(ctrlCmdGetFamily, 0, attrs(nil).str(ctrlAttrFamilyName, familyName))
	if err != nil || len(r) == 0 {
		c.close()
		if err == nil {
			err = errors.New("no reply")
		}
		return nil, fmt.Errorf("generic netlink family %q (is the melinoe-vpn module loaded?): %w", familyName, err)
	}
	id := r[0].attrs.bytes(ctrlAttrFamilyID)
	if len(id) < 2 {
		c.close()
		return nil, fmt.Errorf("generic netlink family %q: no id", familyName)
	}
	c.family = ne.Uint16(id)
	return c, nil
}

func (c *conn) close() {
	if c.closed.Swap(true) {
		return
	}
	<-c.readDone
	unix.Close(c.fd)
}

// request sends a command and waits for everything it brings back (replies, then the ack or
// the end of a dump).
func (c *conn) request(cmd uint8, flags uint16, a attrs) ([]msg, error) {
	if c.closed.Load() {
		return nil, errors.New("netlink: closed")
	}
	seq := c.seq.Add(1)
	l := unix.SizeofNlMsghdr + genlHdrLen + len(a)
	b := make([]byte, l)
	ne.PutUint32(b[0:], uint32(l))
	ne.PutUint16(b[4:], c.family)
	ne.PutUint16(b[6:], unix.NLM_F_REQUEST|unix.NLM_F_ACK|flags)
	ne.PutUint32(b[8:], seq)
	b[unix.SizeofNlMsghdr] = cmd
	b[unix.SizeofNlMsghdr+1] = 1
	copy(b[unix.SizeofNlMsghdr+genlHdrLen:], a)
	ch := make(chan reply, 1)
	c.mu.Lock()
	c.pending[seq] = ch
	c.mu.Unlock()
	if err := unix.Sendto(c.fd, b, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		return nil, fmt.Errorf("netlink send: %w", err)
	}
	select {
	case r := <-ch:
		return r.msgs, r.err
	case <-c.readDone:
		return nil, fmt.Errorf("netlink: %w", c.readErr)
	case <-time.After(10 * time.Second):
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		return nil, errors.New("netlink: no reply")
	}
}

func (c *conn) do(cmd uint8, flags uint16, a attrs) ([]msg, error) { return c.request(cmd, flags, a) }

func (c *conn) dump(cmd uint8, a attrs) ([]msg, error) {
	return c.request(cmd, unix.NLM_F_DUMP, a)
}

func (c *conn) finish(seq uint32, err error) {
	c.mu.Lock()
	ch := c.pending[seq]
	msgs := c.partial[seq]
	delete(c.pending, seq)
	delete(c.partial, seq)
	c.mu.Unlock()
	if ch != nil {
		ch <- reply{msgs: msgs, err: err}
	}
}

func (c *conn) read() {
	defer close(c.readDone)
	buf := make([]byte, 1<<16)
	for !c.closed.Load() {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			// Our receive buffer overflowed: events were lost, the kernel's state is still
			// right (liveness events carry a sequence number to tell).
			if errors.Is(err, unix.ENOBUFS) {
				continue
			}
			c.readErr = err
			return
		}
		c.parse(buf[:n])
	}
	c.readErr = errors.New("closed")
}

func (c *conn) parse(b []byte) {
	for len(b) >= unix.SizeofNlMsghdr {
		l := int(ne.Uint32(b[0:]))
		if l < unix.SizeofNlMsghdr || l > len(b) {
			return
		}
		typ := ne.Uint16(b[4:])
		seq := ne.Uint32(b[8:])
		body := b[unix.SizeofNlMsghdr:l]
		switch {
		case typ == unix.NLMSG_ERROR:
			if len(body) >= 4 {
				errno := -int32(ne.Uint32(body))
				var err error
				if errno != 0 {
					err = &errnoError{errno: unix.Errno(errno), msg: extackMsg(body)}
				}
				c.finish(seq, err)
			}
		case typ == unix.NLMSG_DONE:
			c.finish(seq, nil)
		case len(body) >= genlHdrLen:
			// A copy: the attributes are slices of it, and the read buffer is reused.
			body = append([]byte(nil), body...)
			m := msg{cmd: body[0], attrs: parseAttrs(body[genlHdrLen:])}
			if seq == 0 {
				if typ == c.family && c.onEvent != nil {
					c.onEvent(m)
				}
			} else {
				c.mu.Lock()
				if _, ok := c.pending[seq]; ok {
					c.partial[seq] = append(c.partial[seq], m)
				}
				c.mu.Unlock()
			}
		}
		if align4(l) >= len(b) {
			return
		}
		b = b[align4(l):]
	}
}

// extackMsg is the kernel's error message in an NLMSG_ERROR, if it sent one.
func extackMsg(body []byte) string {
	// error(4) + the request's header (16) + extended ack attributes (we sent no payload
	// back: NETLINK_CAP_ACK is off, so the whole request follows; skip it).
	if len(body) < 4+unix.SizeofNlMsghdr {
		return ""
	}
	reqLen := int(ne.Uint32(body[4:]))
	off := 4 + align4(reqLen)
	if off > len(body) {
		off = 4 + unix.SizeofNlMsghdr
	}
	p := parseAttrs(body[off:])
	if s := p.bytes(unix.NLMSGERR_ATTR_MSG); len(s) > 0 {
		if s[len(s)-1] == 0 {
			s = s[:len(s)-1]
		}
		return string(s)
	}
	return ""
}
