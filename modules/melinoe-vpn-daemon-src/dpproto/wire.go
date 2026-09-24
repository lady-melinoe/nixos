// Package dpproto is the protocol between melnode's two halves: the data
// plane (melnode-dp: the WireGuard-like "kernel" side) and the control
// plane (melnode-cp: liveness, path-vector routing, host integration).
//
// The relationship is the one between an OVS/OVN-style kernel datapath and
// its userspace daemon, just with both halves in userspace for now:
//
//   - The data plane owns everything per-packet: the UDP socket, the Noise
//     handshakes and keypairs, encryption, the tun devices and the
//     dst-peerid -> next-hop link table. It forwards proto=0 (tunneled IP)
//     packets entirely on its own.
//   - Any packet with a non-zero proto in the 4-byte routing header is a
//     control packet. The data plane never interprets it: it "punts" it up
//     to the control plane (Punt), and sends whatever the control plane
//     hands it (Inject).
//   - The control plane puppets the data plane: it adds/removes links, asks
//     for tuns to be created/started/destroyed, and installs/changes/removes
//     next-hop routes. All of that is request/reply (Call).
//
// Transport is one SOCK_SEQPACKET unix socket: message boundaries are
// preserved (so no length framing is needed), delivery is reliable and
// ordered, and a peer going away is an immediate EOF, which is what makes
// "the control plane died" cheap to notice.
//
// Every message is
//
//	type u8 | id u32 | body...
//
// (big endian). id is chosen by the requester and echoed in the reply; it is
// 0 on messages that have no reply (Punt, Inject).
package dpproto

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Version is bumped on any incompatible change to this protocol. The control
// plane sends it in Hello and the data plane refuses a mismatch.
const Version = 1

const (
	// HeaderLen is the fixed message header: type u8 + id u32.
	HeaderLen = 5
	// MaxMessage bounds one datagram: a full 64KiB payload plus headers.
	MaxMessage = 65535 + 64
)

// Type is a message type.
type Type uint8

const (
	// Requests, control plane -> data plane. Each gets exactly one Reply or
	// ReplyErr carrying the same id.
	TypeHello      Type = 1
	TypeLinkAdd    Type = 2
	TypeLinkDel    Type = 3
	TypeLinkList   Type = 4
	TypeTunCreate  Type = 5
	TypeTunStart   Type = 6
	TypeTunDestroy Type = 7
	TypeTunList    Type = 8
	TypeRouteSet   Type = 9
	TypeRouteDel   Type = 10
	TypeRouteList  Type = 11

	// Replies, data plane -> control plane.
	TypeReply    Type = 0x80
	TypeReplyErr Type = 0x81

	// Asynchronous, no reply.
	TypePunt   Type = 0x90 // data plane -> control plane
	TypeInject Type = 0x91 // control plane -> data plane
)

// Error codes carried in ReplyErr.
const (
	CodeInvalid     uint16 = 1 // malformed or nonsensical request
	CodeNotFound    uint16 = 2
	CodeExists      uint16 = 3
	CodeInternal    uint16 = 4
	CodeUnsupported uint16 = 5 // unknown message type / version mismatch
)

// Error is a failure reported by the data plane for one request.
type Error struct {
	Code uint16
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("data plane: %s (code %d)", e.Msg, e.Code) }

// Errorf builds an *Error for a Handler to return.
func Errorf(code uint16, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err is (or wraps) a data-plane Error with this code.
func IsCode(err error, code uint16) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

var errShort = errors.New("dpproto: truncated message")

// ---- messages ---------------------------------------------------------------

// Hello opens a session. The data plane answers with HelloReply.
type Hello struct {
	Version uint16
}

// HelloReply describes the data plane the control plane just attached to.
type HelloReply struct {
	Version uint16
	LocalID uint32
	PubKey  [32]byte // this node's static public key
	MTU     uint32   // MTU of every tun; also the largest payload the control plane should Inject
	Port    uint16   // UDP port the data plane listens on
}

// LinkAdd configures one directly-connected neighbor (a Noise tunnel). It is
// idempotent: adding an existing link with the same PubKey just re-applies
// Endpoint (an empty Endpoint leaves the current one alone, since a
// listen-only link learns its peer's address from traffic); adding it with a
// different PubKey fails with CodeExists (LinkDel it first).
type LinkAdd struct {
	PeerID   uint32
	PubKey   [32]byte
	Endpoint string // ip:port literal, or "" for listen-only
}

// LinkInfo is one link as the data plane sees it.
type LinkInfo struct {
	PeerID                uint32
	PubKey                [32]byte
	Endpoint              string // current remote address ("" if none known yet)
	LastHandshakeUnixNano int64  // 0 = never
	TxBytes               uint64
	RxBytes               uint64
}

// TunCreateReply is returned by TunCreate. Started tells the control plane
// whether the tun already existed and is carrying traffic (so it need not be
// set up again).
type TunCreateReply struct {
	Name    string
	Started bool
}

// TunInfo is one tun as the data plane sees it.
type TunInfo struct {
	PeerID  uint32 // the destination this tun leads to
	Name    string
	MTU     uint32
	Started bool
}

// Route sends traffic for destination Dst out over the link NextHop.
type Route struct {
	Dst     uint32
	NextHop uint32
}

// Punt is a control packet the data plane received and did not handle.
// Payload is everything after the 4-byte routing header, still including any
// trailing padding: parsing (Vers/Length fields and so on) is the control
// plane's business.
type Punt struct {
	Ingress uint32 // link peerid it arrived on
	Proto   uint8
	Src     uint8
	Dst     uint8
	TTL     uint8
	Payload []byte
}

// Inject asks the data plane to send a control packet out over one link. The
// data plane stamps src = its own id and sends it in the priority (control)
// class, ahead of any queued data.
type Inject struct {
	Link    uint32
	Proto   uint8
	Dst     uint8
	TTL     uint8
	Payload []byte
}

// ---- codec ------------------------------------------------------------------

type enc struct{ b []byte }

func (e *enc) u8(v uint8)   { e.b = append(e.b, v) }
func (e *enc) u16(v uint16) { e.b = binary.BigEndian.AppendUint16(e.b, v) }
func (e *enc) u32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *enc) u64(v uint64) { e.b = binary.BigEndian.AppendUint64(e.b, v) }
func (e *enc) raw(v []byte) { e.b = append(e.b, v...) }
func (e *enc) str(s string) {
	if len(s) > 0xffff {
		s = s[:0xffff]
	}
	e.u16(uint16(len(s)))
	e.b = append(e.b, s...)
}

type dec struct {
	b   []byte
	err error
}

func (d *dec) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if len(d.b) < n {
		d.err = errShort
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}
func (d *dec) u8() uint8 {
	if v := d.take(1); v != nil {
		return v[0]
	}
	return 0
}
func (d *dec) u16() uint16 {
	if v := d.take(2); v != nil {
		return binary.BigEndian.Uint16(v)
	}
	return 0
}
func (d *dec) u32() uint32 {
	if v := d.take(4); v != nil {
		return binary.BigEndian.Uint32(v)
	}
	return 0
}
func (d *dec) u64() uint64 {
	if v := d.take(8); v != nil {
		return binary.BigEndian.Uint64(v)
	}
	return 0
}
func (d *dec) key() (k [32]byte) {
	if v := d.take(32); v != nil {
		copy(k[:], v)
	}
	return
}
func (d *dec) str() string {
	n := int(d.u16())
	return string(d.take(n))
}

// rest returns a copy of everything left (a copy, since the receive buffer is reused).
func (d *dec) rest() []byte {
	if d.err != nil {
		return nil
	}
	out := append([]byte(nil), d.b...)
	d.b = nil
	return out
}

// done reports a decode error, including trailing garbage.
func (d *dec) done() error {
	if d.err != nil {
		return d.err
	}
	if len(d.b) != 0 {
		return fmt.Errorf("dpproto: %d trailing bytes", len(d.b))
	}
	return nil
}

func (m Hello) Marshal() []byte { e := enc{}; e.u16(m.Version); return e.b }
func UnmarshalHello(b []byte) (m Hello, err error) {
	d := dec{b: b}
	m.Version = d.u16()
	return m, d.done()
}

func (m HelloReply) Marshal() []byte {
	e := enc{}
	e.u16(m.Version)
	e.u32(m.LocalID)
	e.raw(m.PubKey[:])
	e.u32(m.MTU)
	e.u16(m.Port)
	return e.b
}
func UnmarshalHelloReply(b []byte) (m HelloReply, err error) {
	d := dec{b: b}
	m.Version = d.u16()
	m.LocalID = d.u32()
	m.PubKey = d.key()
	m.MTU = d.u32()
	m.Port = d.u16()
	return m, d.done()
}

func (m LinkAdd) Marshal() []byte {
	e := enc{}
	e.u32(m.PeerID)
	e.raw(m.PubKey[:])
	e.str(m.Endpoint)
	return e.b
}
func UnmarshalLinkAdd(b []byte) (m LinkAdd, err error) {
	d := dec{b: b}
	m.PeerID = d.u32()
	m.PubKey = d.key()
	m.Endpoint = d.str()
	return m, d.done()
}

// MarshalID / UnmarshalID encode the single-peerid bodies (LinkDel, TunCreate,
// TunStart, TunDestroy, RouteDel).
func MarshalID(id uint32) []byte { e := enc{}; e.u32(id); return e.b }
func UnmarshalID(b []byte) (uint32, error) {
	d := dec{b: b}
	id := d.u32()
	return id, d.done()
}

func (m LinkInfo) marshalTo(e *enc) {
	e.u32(m.PeerID)
	e.raw(m.PubKey[:])
	e.str(m.Endpoint)
	e.u64(uint64(m.LastHandshakeUnixNano))
	e.u64(m.TxBytes)
	e.u64(m.RxBytes)
}
func (m *LinkInfo) unmarshalFrom(d *dec) {
	m.PeerID = d.u32()
	m.PubKey = d.key()
	m.Endpoint = d.str()
	m.LastHandshakeUnixNano = int64(d.u64())
	m.TxBytes = d.u64()
	m.RxBytes = d.u64()
}

func MarshalLinkList(l []LinkInfo) []byte {
	e := enc{}
	e.u32(uint32(len(l)))
	for _, x := range l {
		x.marshalTo(&e)
	}
	return e.b
}
func UnmarshalLinkList(b []byte) ([]LinkInfo, error) {
	d := dec{b: b}
	n := d.u32()
	var out []LinkInfo
	for i := uint32(0); i < n && d.err == nil; i++ {
		var x LinkInfo
		x.unmarshalFrom(&d)
		out = append(out, x)
	}
	return out, d.done()
}

func (m TunCreateReply) Marshal() []byte {
	e := enc{}
	e.str(m.Name)
	if m.Started {
		e.u8(1)
	} else {
		e.u8(0)
	}
	return e.b
}
func UnmarshalTunCreateReply(b []byte) (m TunCreateReply, err error) {
	d := dec{b: b}
	m.Name = d.str()
	m.Started = d.u8() != 0
	return m, d.done()
}

func MarshalTunList(l []TunInfo) []byte {
	e := enc{}
	e.u32(uint32(len(l)))
	for _, x := range l {
		e.u32(x.PeerID)
		e.str(x.Name)
		e.u32(x.MTU)
		if x.Started {
			e.u8(1)
		} else {
			e.u8(0)
		}
	}
	return e.b
}
func UnmarshalTunList(b []byte) ([]TunInfo, error) {
	d := dec{b: b}
	n := d.u32()
	var out []TunInfo
	for i := uint32(0); i < n && d.err == nil; i++ {
		var x TunInfo
		x.PeerID = d.u32()
		x.Name = d.str()
		x.MTU = d.u32()
		x.Started = d.u8() != 0
		out = append(out, x)
	}
	return out, d.done()
}

func (m Route) Marshal() []byte { e := enc{}; e.u32(m.Dst); e.u32(m.NextHop); return e.b }
func UnmarshalRoute(b []byte) (m Route, err error) {
	d := dec{b: b}
	m.Dst = d.u32()
	m.NextHop = d.u32()
	return m, d.done()
}

func MarshalRouteList(l []Route) []byte {
	e := enc{}
	e.u32(uint32(len(l)))
	for _, x := range l {
		e.u32(x.Dst)
		e.u32(x.NextHop)
	}
	return e.b
}
func UnmarshalRouteList(b []byte) ([]Route, error) {
	d := dec{b: b}
	n := d.u32()
	var out []Route
	for i := uint32(0); i < n && d.err == nil; i++ {
		out = append(out, Route{Dst: d.u32(), NextHop: d.u32()})
	}
	return out, d.done()
}

// MarshalMessage is the full wire form of a Punt (header included), so the
// data plane builds it in one allocation.
func (m Punt) MarshalMessage() []byte {
	e := enc{b: make([]byte, 0, HeaderLen+8+len(m.Payload))}
	e.u8(uint8(TypePunt))
	e.u32(0)
	e.u32(m.Ingress)
	e.u8(m.Proto)
	e.u8(m.Src)
	e.u8(m.Dst)
	e.u8(m.TTL)
	e.raw(m.Payload)
	return e.b
}
func UnmarshalPunt(b []byte) (m Punt, err error) {
	d := dec{b: b}
	m.Ingress = d.u32()
	m.Proto = d.u8()
	m.Src = d.u8()
	m.Dst = d.u8()
	m.TTL = d.u8()
	m.Payload = d.rest()
	return m, d.done()
}

func (m Inject) Marshal() []byte {
	e := enc{b: make([]byte, 0, 7+len(m.Payload))}
	e.u32(m.Link)
	e.u8(m.Proto)
	e.u8(m.Dst)
	e.u8(m.TTL)
	e.raw(m.Payload)
	return e.b
}
func UnmarshalInject(b []byte) (m Inject, err error) {
	d := dec{b: b}
	m.Link = d.u32()
	m.Proto = d.u8()
	m.Dst = d.u8()
	m.TTL = d.u8()
	m.Payload = d.rest()
	return m, d.done()
}

func marshalError(e *Error) []byte {
	x := enc{}
	x.u16(e.Code)
	x.str(e.Msg)
	return x.b
}
func unmarshalError(b []byte) *Error {
	d := dec{b: b}
	e := &Error{Code: d.u16(), Msg: d.str()}
	if d.err != nil {
		return &Error{Code: CodeInternal, Msg: "malformed error reply"}
	}
	return e
}
