package dpproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// CallTimeout bounds how long Client.Call waits for a reply. The data plane
// answers from memory (the slowest request is creating a tun), so hitting
// this means the data plane is wedged; the session is then dropped.
const CallTimeout = 5 * time.Second

// puntQueueSize is how many punted packets may wait for the control plane to
// read them before new ones are dropped. Control traffic is low-rate and
// loss-tolerant (liveness and path-vector both retransmit by design).
const puntQueueSize = 1024

// conn is one end of the seqpacket socket.
type conn struct {
	c   *net.UnixConn
	buf []byte // receive buffer; only the single reader goroutine touches it
}

func newConn(c *net.UnixConn) *conn { return &conn{c: c, buf: make([]byte, MaxMessage)} }

func (c *conn) send(t Type, id uint32, body []byte) error {
	msg := make([]byte, HeaderLen+len(body))
	msg[0] = byte(t)
	binary.BigEndian.PutUint32(msg[1:], id)
	copy(msg[HeaderLen:], body)
	return c.sendRaw(msg)
}

func (c *conn) sendRaw(msg []byte) error {
	_, err := c.c.Write(msg)
	return err
}

// recv returns the next message. body aliases the receive buffer and is only
// valid until the next recv.
func (c *conn) recv() (t Type, id uint32, body []byte, err error) {
	n, err := c.c.Read(c.buf)
	if err != nil {
		return 0, 0, nil, err
	}
	if n < HeaderLen {
		return 0, 0, nil, errShort
	}
	return Type(c.buf[0]), binary.BigEndian.Uint32(c.buf[1:HeaderLen]), c.buf[HeaderLen:n], nil
}

// ---- client (control plane side) ------------------------------------------------

type reply struct {
	body []byte
	err  error
}

// Client is the control plane's handle on the data plane. It is safe for
// concurrent use.
type Client struct {
	c      *conn
	nextID atomic.Uint32
	onPunt func(Punt)

	mu      sync.Mutex
	pending map[uint32]chan reply

	done    chan struct{}
	errOnce sync.Once
	err     error
}

// Dial connects to the data plane's socket. onPunt is called, from the
// client's single reader goroutine and in arrival order, for every punted
// packet; it must not block (queue and return).
func Dial(path string, onPunt func(Punt)) (*Client, error) {
	uc, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: path, Net: "unixpacket"})
	if err != nil {
		return nil, err
	}
	cl := &Client{c: newConn(uc), onPunt: onPunt, pending: make(map[uint32]chan reply), done: make(chan struct{})}
	go cl.readLoop()
	return cl, nil
}

func (cl *Client) fail(err error) {
	cl.errOnce.Do(func() {
		cl.err = err
		cl.c.c.Close()
		close(cl.done)
		cl.mu.Lock()
		for id, ch := range cl.pending {
			ch <- reply{err: err}
			delete(cl.pending, id)
		}
		cl.mu.Unlock()
	})
}

// Done is closed when the session ends (data plane went away, or Close).
func (cl *Client) Done() <-chan struct{} { return cl.done }

// Err is why the session ended; only meaningful once Done is closed.
func (cl *Client) Err() error { return cl.err }

// Close ends the session.
func (cl *Client) Close() { cl.fail(errors.New("dpproto: client closed")) }

func (cl *Client) readLoop() {
	for {
		t, id, body, err := cl.c.recv()
		if err != nil {
			cl.fail(fmt.Errorf("dpproto: session lost: %w", err))
			return
		}
		switch t {
		case TypeReply, TypeReplyErr:
			r := reply{body: append([]byte(nil), body...)}
			if t == TypeReplyErr {
				r = reply{err: unmarshalError(body)}
			}
			cl.mu.Lock()
			ch, ok := cl.pending[id]
			delete(cl.pending, id)
			cl.mu.Unlock()
			if ok {
				ch <- r // buffered, never blocks
			}
		case TypePunt:
			p, err := UnmarshalPunt(body)
			if err != nil {
				continue // malformed punt: drop, keep the session
			}
			if cl.onPunt != nil {
				cl.onPunt(p)
			}
		}
	}
}

// Call sends one request and waits for its reply body.
func (cl *Client) Call(t Type, body []byte) ([]byte, error) {
	id := cl.nextID.Add(1)
	if id == 0 { // wrapped; 0 is reserved for "no reply expected"
		id = cl.nextID.Add(1)
	}
	ch := make(chan reply, 1)
	cl.mu.Lock()
	select {
	case <-cl.done:
		cl.mu.Unlock()
		return nil, cl.err
	default:
	}
	cl.pending[id] = ch
	cl.mu.Unlock()

	if err := cl.c.send(t, id, body); err != nil {
		cl.fail(fmt.Errorf("dpproto: send: %w", err))
		return nil, cl.err
	}
	timer := time.NewTimer(CallTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.body, r.err
	case <-timer.C:
		cl.mu.Lock()
		delete(cl.pending, id)
		cl.mu.Unlock()
		cl.fail(fmt.Errorf("dpproto: no reply to request type %d within %v", t, CallTimeout))
		return nil, cl.err
	}
}

// Inject sends a control packet for the data plane to transmit. There is no
// reply: like a NIC transmit it is best-effort, and an error means only that
// the session is gone.
func (cl *Client) Inject(i Inject) error {
	select {
	case <-cl.done:
		return cl.err
	default:
	}
	return cl.c.send(TypeInject, 0, i.Marshal())
}

// Typed wrappers around Call.

func (cl *Client) Hello() (HelloReply, error) {
	b, err := cl.Call(TypeHello, Hello{Version: Version}.Marshal())
	if err != nil {
		return HelloReply{}, err
	}
	return UnmarshalHelloReply(b)
}

func (cl *Client) LinkAdd(m LinkAdd) error {
	_, err := cl.Call(TypeLinkAdd, m.Marshal())
	return err
}

func (cl *Client) LinkDel(peerID uint32) error {
	_, err := cl.Call(TypeLinkDel, MarshalID(peerID))
	return err
}

func (cl *Client) LinkList() ([]LinkInfo, error) {
	b, err := cl.Call(TypeLinkList, nil)
	if err != nil {
		return nil, err
	}
	return UnmarshalLinkList(b)
}

// TunCreate makes the tun for destination peerID (idempotent). A new tun is
// created but NOT started: it carries no traffic until TunStart, so the
// control plane can address it and run its hooks first.
func (cl *Client) TunCreate(peerID uint32) (TunCreateReply, error) {
	b, err := cl.Call(TypeTunCreate, MarshalID(peerID))
	if err != nil {
		return TunCreateReply{}, err
	}
	return UnmarshalTunCreateReply(b)
}

func (cl *Client) TunStart(peerID uint32) error {
	_, err := cl.Call(TypeTunStart, MarshalID(peerID))
	return err
}

// TunDestroy closes and removes the tun (idempotent: a missing tun is not an error).
func (cl *Client) TunDestroy(peerID uint32) error {
	_, err := cl.Call(TypeTunDestroy, MarshalID(peerID))
	return err
}

func (cl *Client) TunList() ([]TunInfo, error) {
	b, err := cl.Call(TypeTunList, nil)
	if err != nil {
		return nil, err
	}
	return UnmarshalTunList(b)
}

// RouteSet installs or changes the next hop for a destination.
func (cl *Client) RouteSet(r Route) error {
	_, err := cl.Call(TypeRouteSet, r.Marshal())
	return err
}

// RouteDel removes a destination's route (idempotent).
func (cl *Client) RouteDel(dst uint32) error {
	_, err := cl.Call(TypeRouteDel, MarshalID(dst))
	return err
}

func (cl *Client) RouteList() ([]Route, error) {
	b, err := cl.Call(TypeRouteList, nil)
	if err != nil {
		return nil, err
	}
	return UnmarshalRouteList(b)
}

// ---- server (data plane side) ---------------------------------------------------

// Handler is what the data plane implements. Requests from one session are
// dispatched serially, in order. Returning an *Error (see Errorf) sends that
// code back; any other error becomes CodeInternal.
type Handler interface {
	Hello(Hello) (HelloReply, error)
	LinkAdd(LinkAdd) error
	LinkDel(peerID uint32) error
	LinkList() ([]LinkInfo, error)
	TunCreate(peerID uint32) (TunCreateReply, error)
	TunStart(peerID uint32) error
	TunDestroy(peerID uint32) error
	TunList() ([]TunInfo, error)
	RouteSet(Route) error
	RouteDel(dst uint32) error
	RouteList() ([]Route, error)
	// Inject must not block: it is called from the session's reader.
	Inject(Inject)
}

// Server accepts the control plane on a unix seqpacket socket. Only one
// session is live at a time: a new connection replaces (closes) the previous
// one, like a single OpenFlow controller.
type Server struct {
	l *net.UnixListener
	h Handler

	mu  sync.Mutex
	cur *session

	puntDropped atomic.Uint64
}

// NewServer listens on path (replacing a stale socket file), mode 0600.
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

// Run accepts sessions until Close. Call it in its own goroutine.
func (s *Server) Run() {
	for {
		uc, err := s.l.AcceptUnix()
		if err != nil {
			return
		}
		sess := &session{srv: s, c: newConn(uc), punts: make(chan []byte, puntQueueSize), done: make(chan struct{})}
		s.mu.Lock()
		old := s.cur
		s.cur = sess
		s.mu.Unlock()
		if old != nil {
			old.close()
		}
		go sess.writeLoop()
		go sess.readLoop()
	}
}

// Close stops accepting and drops the current session.
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

// Connected reports whether a control plane session is attached.
func (s *Server) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur != nil
}

// PuntDropped counts punts dropped because no control plane was attached or
// its queue was full.
func (s *Server) PuntDropped() uint64 { return s.puntDropped.Load() }

// Punt hands a control packet to the control plane. It never blocks: with no
// session attached, or a full queue, the packet is dropped (and counted).
func (s *Server) Punt(p Punt) {
	s.mu.Lock()
	cur := s.cur
	s.mu.Unlock()
	if cur == nil {
		s.puntDropped.Add(1)
		return
	}
	select {
	case cur.punts <- p.MarshalMessage():
	default:
		s.puntDropped.Add(1)
	}
}

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
			if err := ss.c.sendRaw(msg); err != nil {
				ss.close()
				return
			}
		}
	}
}

func (ss *session) readLoop() {
	defer ss.close()
	h := ss.srv.h
	for {
		t, id, body, err := ss.c.recv()
		if err != nil {
			return
		}
		if t == TypeInject {
			if m, err := UnmarshalInject(body); err == nil {
				h.Inject(m)
			}
			continue
		}
		out, err := dispatch(h, t, body)
		var rerr error
		if err != nil {
			e, ok := err.(*Error)
			if !ok {
				e = &Error{Code: CodeInternal, Msg: err.Error()}
			}
			rerr = ss.c.send(TypeReplyErr, id, marshalError(e))
		} else {
			rerr = ss.c.send(TypeReply, id, out)
		}
		if rerr != nil {
			return
		}
	}
}

func dispatch(h Handler, t Type, body []byte) ([]byte, error) {
	bad := func(err error) (*Error, bool) {
		if err != nil {
			return Errorf(CodeInvalid, "malformed request: %v", err), true
		}
		return nil, false
	}
	switch t {
	case TypeHello:
		m, err := UnmarshalHello(body)
		if e, ok := bad(err); ok {
			return nil, e
		}
		if m.Version != Version {
			return nil, Errorf(CodeUnsupported, "control plane speaks protocol v%d, data plane v%d", m.Version, Version)
		}
		r, err := h.Hello(m)
		if err != nil {
			return nil, err
		}
		return r.Marshal(), nil
	case TypeLinkAdd:
		m, err := UnmarshalLinkAdd(body)
		if e, ok := bad(err); ok {
			return nil, e
		}
		return nil, h.LinkAdd(m)
	case TypeLinkDel, TypeTunStart, TypeTunDestroy, TypeRouteDel, TypeTunCreate:
		id, err := UnmarshalID(body)
		if e, ok := bad(err); ok {
			return nil, e
		}
		switch t {
		case TypeLinkDel:
			return nil, h.LinkDel(id)
		case TypeTunStart:
			return nil, h.TunStart(id)
		case TypeTunDestroy:
			return nil, h.TunDestroy(id)
		case TypeRouteDel:
			return nil, h.RouteDel(id)
		default:
			r, err := h.TunCreate(id)
			if err != nil {
				return nil, err
			}
			return r.Marshal(), nil
		}
	case TypeLinkList:
		l, err := h.LinkList()
		if err != nil {
			return nil, err
		}
		return MarshalLinkList(l), nil
	case TypeTunList:
		l, err := h.TunList()
		if err != nil {
			return nil, err
		}
		return MarshalTunList(l), nil
	case TypeRouteSet:
		m, err := UnmarshalRoute(body)
		if e, ok := bad(err); ok {
			return nil, e
		}
		return nil, h.RouteSet(m)
	case TypeRouteList:
		l, err := h.RouteList()
		if err != nil {
			return nil, err
		}
		return MarshalRouteList(l), nil
	}
	return nil, Errorf(CodeUnsupported, "unknown request type %d", t)
}
