package dpproto

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const CallTimeout = 5 * time.Second

const puntQueueSize = 1024

type Datapath interface {
	DeviceSet(DeviceSet) (DeviceSetReply, error)
	Stats() ([]Stat, error)

	LinkSet(LinkSet) error
	LinkDel(peerID uint32) error
	LinkList() ([]LinkInfo, error)

	TunSet(TunSet) (TunInfo, error)
	TunDel(peerID uint32) error
	TunList() ([]TunInfo, error)

	RouteSet([]Route) error
	RouteList() ([]Route, error)

	Inject(Inject) error

	Done() <-chan struct{}
	Err() error
	Close()
}

type rawConn interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}

type conn struct {
	c   rawConn
	buf []byte
}

func newConn(c rawConn) *conn { return &conn{c: c, buf: make([]byte, MaxMessage)} }

func (c *conn) send(msg []byte) error {
	_, err := c.c.Write(msg)
	return err
}

func (c *conn) recv() ([]nlmsg, error) {
	n, err := c.c.Read(c.buf)
	if err != nil {
		return nil, err
	}
	return splitMessages(c.buf[:n])
}

type result struct {
	parts []attrs
	err   error
}

type call struct {
	parts []attrs
	ch    chan result
}

type Client struct {
	c       *conn
	family  uint16
	nextSeq atomic.Uint32
	onPunt  func(Punt)

	mu      sync.Mutex
	pending map[uint32]*call

	done    chan struct{}
	errOnce sync.Once
	err     error
}

var _ Datapath = (*Client)(nil)

func Dial(path string, onPunt func(Punt)) (*Client, error) {
	uc, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	cl := &Client{c: newConn(uc), onPunt: onPunt, pending: make(map[uint32]*call), done: make(chan struct{})}
	go cl.readLoop()
	if err := cl.resolveFamily(); err != nil {
		cl.Close()
		return nil, err
	}
	return cl, nil
}

func DialKernel(onPunt func(Punt)) (*Client, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_GENERIC)
	if err != nil {
		return nil, fmt.Errorf("dpproto: netlink socket: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("dpproto: netlink setnonblock: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("dpproto: netlink bind: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_NO_ENOBUFS, 1); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("dpproto: netlink NETLINK_NO_ENOBUFS: %w", err)
	}
	f := os.NewFile(uintptr(fd), "melnode-genl")

	cl := &Client{c: newConn(f), onPunt: onPunt, pending: make(map[uint32]*call), done: make(chan struct{})}
	go cl.readLoop()
	if err := cl.resolveFamily(); err != nil {
		cl.Close()
		return nil, err
	}
	return cl, nil
}

func (cl *Client) resolveFamily() error {
	parts, err := cl.request(genlIDCtrl, ctrlCmdGetFamily, ctrlVersion, false, func(b *nlb) error {
		b.str(ctrlAttrFamilyName, FamilyName)
		return nil
	})
	if err != nil {
		return fmt.Errorf("resolving netlink family %q: %w", FamilyName, err)
	}
	if len(parts) != 1 || !parts[0].has(ctrlAttrFamilyID) {
		return fmt.Errorf("resolving netlink family %q: malformed reply", FamilyName)
	}
	a := parts[0]
	if v := a.u32(ctrlAttrVersion); v != uint32(Version) {
		return fmt.Errorf("data plane implements %s API v%d, control plane v%d", FamilyName, v, Version)
	}
	cl.family = a.u16(ctrlAttrFamilyID)
	return nil
}

func (cl *Client) fail(err error) {
	cl.errOnce.Do(func() {
		cl.err = err
		cl.c.c.Close()
		close(cl.done)
		cl.mu.Lock()
		for seq, p := range cl.pending {
			p.ch <- result{err: err}
			delete(cl.pending, seq)
		}
		cl.mu.Unlock()
	})
}

func (cl *Client) Done() <-chan struct{} { return cl.done }

func (cl *Client) Err() error { return cl.err }

func (cl *Client) Close() { cl.fail(errors.New("dpproto: client closed")) }

func (cl *Client) readLoop() {
	for {
		msgs, err := cl.c.recv()
		if err != nil && len(msgs) == 0 {
			if errors.Is(err, unix.ENOBUFS) {
				continue
			}
			cl.fail(fmt.Errorf("dpproto: session lost: %w", err))
			return
		}
		for _, m := range msgs {
			cl.handle(m)
		}
	}
}

func (cl *Client) handle(m nlmsg) {
	switch {
	case m.typ == nlmsgNoop:
	case m.typ == nlmsgError || m.typ == nlmsgDone:
		cl.mu.Lock()
		p, ok := cl.pending[m.seq]
		delete(cl.pending, m.seq)
		cl.mu.Unlock()
		if !ok {
			return
		}
		r := result{parts: p.parts}
		if m.typ == nlmsgError {
			r.err = parseErrMessage(m)
		}
		p.ch <- r
	case m.typ >= 0x10 && m.seq == 0 && m.cmd == CmdPunt:
		a, err := m.attrs()
		if err != nil {
			return
		}
		var p Punt
		if p.get(a) == nil && cl.onPunt != nil {
			cl.onPunt(p)
		}
	case m.typ >= 0x10:
		body := append([]byte(nil), m.body...)
		a, err := parseAttrs(body[min(genlHdrLen, len(body)):])
		if err != nil {
			return
		}
		cl.mu.Lock()
		if p, ok := cl.pending[m.seq]; ok {
			p.parts = append(p.parts, a)
		}
		cl.mu.Unlock()
	}
}

func (cl *Client) do(cmd uint8, dump bool, put func(*nlb) error) ([]attrs, error) {
	return cl.request(cl.family, cmd, uint8(Version), dump, put)
}

func (cl *Client) request(family uint16, cmd, version uint8, dump bool, put func(*nlb) error) ([]attrs, error) {
	seq := cl.nextSeq.Add(1)
	if seq == 0 {
		seq = cl.nextSeq.Add(1)
	}
	flags := uint16(nlmFRequest | nlmFAck)
	if dump {
		flags = nlmFRequest | nlmFDump
	}
	b := newNL(family, flags, seq, 0, cmd, version)
	if put != nil {
		if err := put(b); err != nil {
			return nil, err
		}
	}
	p := &call{ch: make(chan result, 1)}
	cl.mu.Lock()
	select {
	case <-cl.done:
		cl.mu.Unlock()
		return nil, cl.err
	default:
	}
	cl.pending[seq] = p
	cl.mu.Unlock()

	if err := cl.c.send(b.bytes()); err != nil {
		cl.fail(fmt.Errorf("dpproto: send: %w", err))
		return nil, cl.err
	}
	timer := time.NewTimer(CallTimeout)
	defer timer.Stop()
	select {
	case r := <-p.ch:
		return r.parts, r.err
	case <-timer.C:
		cl.mu.Lock()
		delete(cl.pending, seq)
		cl.mu.Unlock()
		cl.fail(fmt.Errorf("dpproto: no reply to command %d within %v", cmd, CallTimeout))
		return nil, cl.err
	}
}

func (cl *Client) one(cmd uint8, put func(*nlb) error) (attrs, error) {
	parts, err := cl.do(cmd, false, put)
	if err != nil {
		return nil, err
	}
	if len(parts) != 1 {
		return nil, fmt.Errorf("dpproto: command %d: expected one reply message, got %d", cmd, len(parts))
	}
	return parts[0], nil
}

func (cl *Client) ack(cmd uint8, put func(*nlb) error) error {
	_, err := cl.do(cmd, false, put)
	return err
}

func (cl *Client) Inject(i Inject) error {
	select {
	case <-cl.done:
		return cl.err
	default:
	}
	b := newNL(cl.family, nlmFRequest, 0, 0, CmdInject, uint8(Version))
	i.put(b)
	return cl.c.send(b.bytes())
}

func (cl *Client) DeviceSet(m DeviceSet) (DeviceSetReply, error) {
	a, err := cl.one(CmdDeviceSet, func(b *nlb) error { m.put(b); return nil })
	if err != nil {
		return DeviceSetReply{}, err
	}
	var r DeviceSetReply
	return r, r.get(a)
}

func (cl *Client) Stats() ([]Stat, error) {
	parts, err := cl.do(CmdStatsGet, true, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Stat, 0, len(parts))
	for _, a := range parts {
		var s Stat
		if err := s.get(a); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (cl *Client) LinkSet(m LinkSet) error {
	return cl.ack(CmdLinkSet, m.put)
}

func (cl *Client) LinkDel(peerID uint32) error {
	return cl.ack(CmdLinkDel, func(b *nlb) error { putID(b, AttrPeerID, peerID); return nil })
}

func (cl *Client) LinkList() ([]LinkInfo, error) {
	parts, err := cl.do(CmdLinkGet, true, nil)
	if err != nil {
		return nil, err
	}
	out := make([]LinkInfo, 0, len(parts))
	for _, a := range parts {
		var x LinkInfo
		if err := x.get(a); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

func (cl *Client) TunSet(m TunSet) (TunInfo, error) {
	a, err := cl.one(CmdTunSet, func(b *nlb) error { m.put(b); return nil })
	if err != nil {
		return TunInfo{}, err
	}
	var r TunInfo
	return r, r.get(a)
}

func (cl *Client) TunDel(peerID uint32) error {
	return cl.ack(CmdTunDel, func(b *nlb) error { putID(b, AttrPeerID, peerID); return nil })
}

func (cl *Client) TunList() ([]TunInfo, error) {
	parts, err := cl.do(CmdTunGet, true, nil)
	if err != nil {
		return nil, err
	}
	out := make([]TunInfo, 0, len(parts))
	for _, a := range parts {
		var x TunInfo
		if err := x.get(a); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

func (cl *Client) RouteSet(routes []Route) error {
	return cl.ack(CmdRouteSet, func(b *nlb) error { putRoutes(b, routes); return nil })
}

func (cl *Client) RouteList() ([]Route, error) {
	parts, err := cl.do(CmdRouteGet, true, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Route, 0, len(parts))
	for _, a := range parts {
		var x Route
		if err := x.get(a); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

type Handler interface {
	DeviceSet(DeviceSet) (DeviceSetReply, error)
	Stats() ([]Stat, error)
	LinkSet(LinkSet) error
	LinkDel(peerID uint32) error
	LinkList() ([]LinkInfo, error)
	TunSet(TunSet) (TunInfo, error)
	TunDel(peerID uint32) error
	TunList() ([]TunInfo, error)
	RouteSet([]Route) error
	RouteList() ([]Route, error)
	Inject(Inject)
	Close()
}

type Factory func(*Session) Handler

type Server struct {
	l       *net.UnixListener
	factory Factory

	mu       sync.Mutex
	sessions map[*Session]struct{}
	closed   bool
	wg       sync.WaitGroup
}

func NewServer(path string, factory Factory) (*Server, error) {
	_ = os.Remove(path)
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return &Server{l: l, factory: factory, sessions: make(map[*Session]struct{})}, nil
}

func (s *Server) Run() {
	for {
		uc, err := s.l.AcceptUnix()
		if err != nil {
			return
		}
		sess := &Session{srv: s, c: newConn(uc), punts: make(chan []byte, puntQueueSize), done: make(chan struct{})}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			uc.Close()
			return
		}
		s.sessions[sess] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		sess.h = s.factory(sess)
		go sess.writeLoop()
		go sess.readLoop()
	}
}

func (s *Server) Close() {
	s.l.Close()
	s.mu.Lock()
	s.closed = true
	live := make([]*Session, 0, len(s.sessions))
	for ss := range s.sessions {
		live = append(live, ss)
	}
	s.mu.Unlock()
	for _, ss := range live {
		ss.close()
	}
	s.wg.Wait()
}

func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

type Session struct {
	srv   *Server
	h     Handler
	c     *conn
	punts chan []byte
	done  chan struct{}
	once  sync.Once

	puntsSent   atomic.Uint64
	puntDropped atomic.Uint64
}

func (ss *Session) Done() <-chan struct{} { return ss.done }

func (ss *Session) PuntsSent() uint64 { return ss.puntsSent.Load() }

func (ss *Session) PuntDropped() uint64 { return ss.puntDropped.Load() }

func (ss *Session) Punt(p Punt) {
	select {
	case <-ss.done:
		ss.puntDropped.Add(1)
		return
	default:
	}
	b := newNL(FamilyID, 0, 0, 0, CmdPunt, uint8(Version))
	b.b = append(make([]byte, 0, HeaderLen+48+len(p.Payload)), b.b...)
	p.put(b)
	select {
	case ss.punts <- b.bytes():
		ss.puntsSent.Add(1)
	default:
		ss.puntDropped.Add(1)
	}
}

func (ss *Session) close() {
	ss.once.Do(func() {
		close(ss.done)
		ss.c.c.Close()
		if ss.h != nil {
			ss.h.Close()
		}
		ss.srv.mu.Lock()
		delete(ss.srv.sessions, ss)
		ss.srv.mu.Unlock()
		ss.srv.wg.Done()
	})
}

func (ss *Session) writeLoop() {
	for {
		select {
		case <-ss.done:
			return
		case msg := <-ss.punts:
			if err := ss.c.send(msg); err != nil {
				ss.close()
				return
			}
		}
	}
}

func (ss *Session) readLoop() {
	defer ss.close()
	for {
		msgs, err := ss.c.recv()
		if err != nil && len(msgs) == 0 {
			return
		}
		for _, m := range msgs {
			if !ss.serve(m) {
				return
			}
		}
	}
}

func (ss *Session) serve(m nlmsg) bool {
	if m.typ < 0x10 || m.flags&nlmFRequest == 0 {
		return true
	}
	if m.typ == genlIDCtrl {
		return ss.serveController(m)
	}
	if m.typ != FamilyID {
		return ss.c.send(errMessage(m, Errorf(CodeNotFound, "no such netlink family %d", m.typ))) == nil
	}
	h := ss.h
	a, perr := m.attrs()
	if m.cmd == CmdInject {
		if perr == nil {
			var i Inject
			if i.get(a) == nil {
				h.Inject(i)
			}
		}
		return true
	}

	var parts []func(*nlb) error
	var herr error
	if perr != nil {
		herr = Errorf(CodeInvalid, "malformed request: %v", perr)
	} else {
		parts, herr = ss.dispatch(m.cmd, a)
	}

	if herr != nil {
		e, ok := herr.(*Error)
		if !ok {
			e = &Error{Code: CodeInternal, Msg: herr.Error()}
		}
		return ss.c.send(errMessage(m, e)) == nil
	}

	dump := m.flags&nlmFDump == nlmFDump
	flags := uint16(0)
	if dump {
		flags = nlmFMulti
	}
	for _, put := range parts {
		b := newNL(m.typ, flags, m.seq, 0, m.cmd, uint8(Version))
		if err := put(b); err != nil {
			return ss.c.send(errMessage(m, Errorf(CodeInternal, "encoding reply: %v", err))) == nil
		}
		if ss.c.send(b.bytes()) != nil {
			return false
		}
	}
	switch {
	case dump:
		done := newNL(nlmsgDone, nlmFMulti, m.seq, 0, 0, 0)
		done.b = nativeEndian.AppendUint32(done.b[:nlmsgHdrLen], 0)
		return ss.c.send(done.bytes()) == nil
	case m.flags&nlmFAck != 0:
		return ss.c.send(errMessage(m, nil)) == nil
	}
	return true
}

func one(put func(*nlb) error) []func(*nlb) error { return []func(*nlb) error{put} }

func (ss *Session) dispatch(cmd uint8, a attrs) ([]func(*nlb) error, error) {
	h := ss.h
	switch cmd {
	case CmdDeviceSet:
		var m DeviceSet
		if err := m.get(a); err != nil {
			return nil, err
		}
		r, err := h.DeviceSet(m)
		if err != nil {
			return nil, err
		}
		return one(func(b *nlb) error { r.put(b); return nil }), nil
	case CmdStatsGet:
		l, err := h.Stats()
		if err != nil {
			return nil, err
		}
		var out []func(*nlb) error
		for _, x := range l {
			out = append(out, func(b *nlb) error { x.put(b); return nil })
		}
		return out, nil
	case CmdLinkSet:
		var m LinkSet
		if err := m.get(a); err != nil {
			return nil, Errorf(CodeInvalid, "%v", err)
		}
		return nil, h.LinkSet(m)
	case CmdLinkDel, CmdTunDel:
		id, err := getID(a, AttrPeerID)
		if err != nil {
			return nil, err
		}
		if cmd == CmdLinkDel {
			return nil, h.LinkDel(id)
		}
		return nil, h.TunDel(id)
	case CmdLinkGet:
		l, err := h.LinkList()
		if err != nil {
			return nil, err
		}
		var out []func(*nlb) error
		for _, x := range l {
			out = append(out, x.put)
		}
		return out, nil
	case CmdTunSet:
		var m TunSet
		if err := m.get(a); err != nil {
			return nil, err
		}
		r, err := h.TunSet(m)
		if err != nil {
			return nil, err
		}
		return one(func(b *nlb) error { r.put(b); return nil }), nil
	case CmdTunGet:
		l, err := h.TunList()
		if err != nil {
			return nil, err
		}
		var out []func(*nlb) error
		for _, x := range l {
			out = append(out, func(b *nlb) error { x.put(b); return nil })
		}
		return out, nil
	case CmdRouteSet:
		routes, err := getRoutes(a)
		if err != nil {
			return nil, err
		}
		return nil, h.RouteSet(routes)
	case CmdRouteGet:
		l, err := h.RouteList()
		if err != nil {
			return nil, err
		}
		var out []func(*nlb) error
		for _, x := range l {
			out = append(out, func(b *nlb) error { x.put(b); return nil })
		}
		return out, nil
	}
	return nil, Errorf(CodeUnsupported, "unknown command %d", cmd)
}

func (ss *Session) serveController(m nlmsg) bool {
	a, err := m.attrs()
	if m.cmd != ctrlCmdGetFamily || err != nil {
		return ss.c.send(errMessage(m, Errorf(CodeUnsupported, "unsupported controller request"))) == nil
	}
	if name := a.str(ctrlAttrFamilyName); name != FamilyName {
		return ss.c.send(errMessage(m, Errorf(CodeNotFound, "no such netlink family %q", name))) == nil
	}
	b := newNL(genlIDCtrl, 0, m.seq, 0, ctrlCmdNewFamily, ctrlVersion)
	b.u16(ctrlAttrFamilyID, FamilyID).str(ctrlAttrFamilyName, FamilyName).u32(ctrlAttrVersion, uint32(Version))
	b.u32(ctrlAttrHdrSize, 0).u32(ctrlAttrMaxAttr, uint32(AttrPktData))
	if ss.c.send(b.bytes()) != nil {
		return false
	}
	if m.flags&nlmFAck != 0 {
		return ss.c.send(errMessage(m, nil)) == nil
	}
	return true
}
