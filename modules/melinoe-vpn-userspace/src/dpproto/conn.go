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
	Hello() (HelloReply, error)
	Attach() error

	DeviceSet(DeviceSet) (DeviceSetReply, error)
	DeviceDel() error
	Stats() ([]Stat, error)

	LinkAdd(LinkAdd) error
	LinkDel(peerID uint32) error
	LinkList() ([]LinkInfo, error)

	TunCreate(peerID uint32, name string) (TunCreateReply, error)
	TunStart(peerID uint32) error
	TunDestroy(peerID uint32) error
	TunList() ([]TunInfo, error)

	RouteSet(Route) error
	RouteDel(dst uint32) error
	RouteList() ([]Route, error)

	Inject(Inject) error

	Done() <-chan struct{}
	Err() error
	Close()
}

type Quitter interface {
	Quit() error
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
	c             *conn
	family        uint16
	eventsGroupID uint32
	isKernel      bool
	nextSeq       atomic.Uint32
	onPunt        func(Punt)
	onEvent       func(Event)

	mu      sync.Mutex
	pending map[uint32]*call

	done    chan struct{}
	errOnce sync.Once
	err     error
}

var (
	_ Datapath = (*Client)(nil)
	_ Quitter  = (*Client)(nil)
)

func Dial(path string, onPunt func(Punt), onEvent func(Event)) (*Client, error) {
	uc, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	cl := &Client{c: newConn(uc), onPunt: onPunt, onEvent: onEvent, pending: make(map[uint32]*call), done: make(chan struct{})}
	go cl.readLoop()
	if err := cl.resolveFamily(); err != nil {
		cl.Close()
		return nil, err
	}
	return cl, nil
}

func DialKernel(onPunt func(Punt), onEvent func(Event)) (*Client, error) {
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

	cl := &Client{c: newConn(f), isKernel: true, onPunt: onPunt, onEvent: onEvent, pending: make(map[uint32]*call), done: make(chan struct{})}
	go cl.readLoop()
	if err := cl.resolveFamily(); err != nil {
		cl.Close()
		return nil, err
	}
	if cl.eventsGroupID != 0 {
		if err := unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_ADD_MEMBERSHIP, int(cl.eventsGroupID)); err != nil {
			cl.Close()
			return nil, fmt.Errorf("dpproto: joining %q multicast group: %w", eventsGroupName, err)
		}
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

	if raw, ok := a.get(ctrlAttrMcastGroups); ok {
		entries, err := parseAttrs(raw)
		if err != nil {
			return fmt.Errorf("resolving netlink family %q: bad multicast group list: %w", FamilyName, err)
		}
		for _, e := range entries {
			g, err := parseAttrs(e.data)
			if err != nil {
				continue
			}
			if g.str(ctrlAttrMcastGrpName) == eventsGroupName {
				cl.eventsGroupID = g.u32(ctrlAttrMcastGrpID)
				break
			}
		}
	}
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
	case m.typ >= 0x10 && m.seq == 0 && (m.cmd == CmdPunt || m.cmd == CmdEvent):
		a, err := m.attrs()
		if err != nil {
			return
		}
		if m.cmd == CmdPunt {
			var p Punt
			if p.get(a) == nil && cl.onPunt != nil {
				cl.onPunt(p)
			}
		} else {
			var e Event
			if e.get(a) == nil && cl.onEvent != nil {
				cl.onEvent(e)
			}
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

func (cl *Client) Hello() (HelloReply, error) {
	a, err := cl.one(CmdHello, func(b *nlb) error { Hello{Version: Version}.put(b); return nil })
	if err != nil {
		return HelloReply{}, err
	}
	var r HelloReply
	return r, r.get(a)
}

func (cl *Client) Attach() error { return cl.ack(CmdAttach, nil) }

func (cl *Client) DeviceSet(m DeviceSet) (DeviceSetReply, error) {
	a, err := cl.one(CmdDeviceSet, func(b *nlb) error { m.put(b); return nil })
	if err != nil {
		return DeviceSetReply{}, err
	}
	var r DeviceSetReply
	return r, r.get(a)
}

func (cl *Client) DeviceDel() error { return cl.ack(CmdDeviceDel, nil) }

func (cl *Client) Quit() error {
	if cl.isKernel {
		return nil
	}
	return cl.ack(CmdXQuit, nil)
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

func (cl *Client) LinkAdd(m LinkAdd) error {
	return cl.ack(CmdLinkAdd, m.put)
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

func (cl *Client) TunCreate(peerID uint32, name string) (TunCreateReply, error) {
	a, err := cl.one(CmdTunCreate, func(b *nlb) error { TunCreate{PeerID: peerID, Name: name}.put(b); return nil })
	if err != nil {
		return TunCreateReply{}, err
	}
	var r TunCreateReply
	return r, r.get(a)
}

func (cl *Client) TunStart(peerID uint32) error {
	return cl.ack(CmdTunStart, func(b *nlb) error { putID(b, AttrPeerID, peerID); return nil })
}

func (cl *Client) TunDestroy(peerID uint32) error {
	return cl.ack(CmdTunDestroy, func(b *nlb) error { putID(b, AttrPeerID, peerID); return nil })
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

func (cl *Client) RouteSet(r Route) error {
	return cl.ack(CmdRouteSet, func(b *nlb) error { r.put(b); return nil })
}

func (cl *Client) RouteDel(dst uint32) error {
	return cl.ack(CmdRouteDel, func(b *nlb) error { putID(b, AttrRouteDst, dst); return nil })
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
	Hello(Hello) (HelloReply, error)
	DeviceSet(DeviceSet) (DeviceSetReply, error)
	DeviceDel() error
	Stats() ([]Stat, error)
	Quit()
	LinkAdd(LinkAdd) error
	LinkDel(peerID uint32) error
	LinkList() ([]LinkInfo, error)
	TunCreate(TunCreate) (TunCreateReply, error)
	TunStart(peerID uint32) error
	TunDestroy(peerID uint32) error
	TunList() ([]TunInfo, error)
	RouteSet(Route) error
	RouteDel(dst uint32) error
	RouteList() ([]Route, error)
	Inject(Inject)
}

type Server struct {
	l *net.UnixListener
	h Handler

	mu  sync.Mutex
	cur *session

	puntDropped   atomic.Uint64
	eventsDropped atomic.Uint64
	puntsSent     atomic.Uint64
}

func NewServer(path string, h Handler) (*Server, error) {
	_ = os.Remove(path)
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return &Server{l: l, h: h}, nil
}

func (s *Server) Run() {
	for {
		uc, err := s.l.AcceptUnix()
		if err != nil {
			return
		}
		sess := &session{srv: s, c: newConn(uc), punts: make(chan []byte, puntQueueSize), done: make(chan struct{})}
		go sess.writeLoop()
		go sess.readLoop()
	}
}

func (s *Server) Close() {
	s.l.Close()
	s.mu.Lock()
	cur := s.cur
	s.cur = nil
	s.mu.Unlock()
	if cur != nil {
		cur.close()
	}
}

func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil
}

func (s *Server) PuntsSent() uint64 { return s.puntsSent.Load() }

func (s *Server) PuntDropped() uint64 { return s.puntDropped.Load() }

func (s *Server) Punt(p Punt) {
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur == nil {
		s.puntDropped.Add(1)
		return
	}
	b := newNL(FamilyID, 0, 0, 0, CmdPunt, uint8(Version))
	b.b = append(make([]byte, 0, HeaderLen+48+len(p.Payload)), b.b...)
	p.put(b)
	select {
	case cur.punts <- b.bytes():
		s.puntsSent.Add(1)
	default:
		s.puntDropped.Add(1)
	}
}

func (s *Server) Event(e Event) {
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur == nil {
		s.eventsDropped.Add(1)
		return
	}
	b := newNL(FamilyID, 0, 0, 0, CmdEvent, uint8(Version))
	if err := e.put(b); err != nil {
		s.eventsDropped.Add(1)
		return
	}
	select {
	case cur.punts <- b.bytes():
	default:
		s.eventsDropped.Add(1)
	}
}

func (s *Server) EventsDropped() uint64 { return s.eventsDropped.Load() }

type session struct {
	srv   *Server
	c     *conn
	punts chan []byte
	done  chan struct{}
	once  sync.Once
}

func (ss *session) close() {
	ss.once.Do(func() {
		close(ss.done)
		ss.c.c.Close()
		ss.srv.mu.Lock()
		if ss.srv.cur == ss {
			ss.srv.cur = nil
		}
		ss.srv.mu.Unlock()
	})
}

func (ss *session) writeLoop() {
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

func (ss *session) attach() {
	ss.srv.mu.Lock()
	old := ss.srv.cur
	ss.srv.cur = ss
	ss.srv.mu.Unlock()
	if old != nil && old != ss {
		old.close()
	}
}

func (ss *session) readLoop() {
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

func (ss *session) serve(m nlmsg) bool {
	if m.typ < 0x10 || m.flags&nlmFRequest == 0 {
		return true
	}
	if m.typ == genlIDCtrl {
		return ss.serveController(m)
	}
	if m.typ != FamilyID {
		return ss.c.send(errMessage(m, Errorf(CodeNotFound, "no such netlink family %d", m.typ))) == nil
	}
	h := ss.srv.h
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
		if ss.c.send(done.bytes()) != nil {
			return false
		}
	case m.flags&nlmFAck != 0:
		if ss.c.send(errMessage(m, nil)) != nil {
			return false
		}
	}
	if m.cmd == CmdXQuit {
		h.Quit()
		return false
	}
	return true
}

func one(put func(*nlb) error) []func(*nlb) error { return []func(*nlb) error{put} }

func (ss *session) dispatch(cmd uint8, a attrs) ([]func(*nlb) error, error) {
	h := ss.srv.h
	switch cmd {
	case CmdHello:
		var m Hello
		if err := m.get(a); err != nil {
			return nil, err
		}
		if m.Version != Version {
			return nil, Errorf(CodeUnsupported, "control plane speaks API v%d, data plane v%d", m.Version, Version)
		}
		r, err := h.Hello(m)
		if err != nil {
			return nil, err
		}
		return one(func(b *nlb) error { r.put(b); return nil }), nil
	case CmdAttach:
		ss.attach()
		return nil, nil
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
	case CmdDeviceDel:
		return nil, h.DeviceDel()
	case CmdXQuit:
		return nil, nil
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
	case CmdLinkAdd:
		var m LinkAdd
		if err := m.get(a); err != nil {
			return nil, Errorf(CodeInvalid, "%v", err)
		}
		return nil, h.LinkAdd(m)
	case CmdLinkDel, CmdTunStart, CmdTunDestroy:
		id, err := getID(a, AttrPeerID)
		if err != nil {
			return nil, err
		}
		switch cmd {
		case CmdLinkDel:
			return nil, h.LinkDel(id)
		case CmdTunStart:
			return nil, h.TunStart(id)
		default:
			return nil, h.TunDestroy(id)
		}
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
	case CmdTunCreate:
		var m TunCreate
		if err := m.get(a); err != nil {
			return nil, err
		}
		r, err := h.TunCreate(m)
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
		var m Route
		if err := m.get(a); err != nil {
			return nil, err
		}
		return nil, h.RouteSet(m)
	case CmdRouteDel:
		id, err := getID(a, AttrRouteDst)
		if err != nil {
			return nil, err
		}
		return nil, h.RouteDel(id)
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

func (ss *session) serveController(m nlmsg) bool {
	a, err := m.attrs()
	if m.cmd != ctrlCmdGetFamily || err != nil {
		return ss.c.send(errMessage(m, Errorf(CodeUnsupported, "unsupported controller request"))) == nil
	}
	if name := a.str(ctrlAttrFamilyName); name != FamilyName {
		return ss.c.send(errMessage(m, Errorf(CodeNotFound, "no such netlink family %q", name))) == nil
	}
	b := newNL(genlIDCtrl, 0, m.seq, 0, ctrlCmdNewFamily, ctrlVersion)
	b.u16(ctrlAttrFamilyID, FamilyID).str(ctrlAttrFamilyName, FamilyName).u32(ctrlAttrVersion, uint32(Version))
	b.u32(ctrlAttrHdrSize, 0).u32(ctrlAttrMaxAttr, uint32(AttrEvtTime))
	b.nest(ctrlAttrMcastGroups, func(b *nlb) {
		b.nest(1, func(b *nlb) {
			b.str(ctrlAttrMcastGrpName, eventsGroupName).u32(ctrlAttrMcastGrpID, eventsGroupID)
		})
	})
	if ss.c.send(b.bytes()) != nil {
		return false
	}
	if m.flags&nlmFAck != 0 {
		return ss.c.send(errMessage(m, nil)) == nil
	}
	return true
}
