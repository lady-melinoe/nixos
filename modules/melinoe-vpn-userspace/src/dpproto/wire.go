// Package dpproto is the protocol between melnode's two halves: the data
// plane (melnode-dp: the WireGuard-like "kernel" side) and the control plane
// (melnode-cp: liveness, path-vector routing, host integration).
//
// The relationship is the one between an OVS-style kernel datapath and its
// userspace daemon, with the kernel half being userspace for now. To make the
// day the data plane becomes a kernel module a backend swap for the control
// plane, the protocol is a generic netlink family ("melnode") in every
// respect but the socket: real nlmsghdr/genlmsghdr/nlattr framing (nl.go),
// commands with attribute sets, ACKs with extended-ack text, multipart dumps,
// unsolicited multicast-style events. API.md and melnode_genl.h are the
// specification; this package is its userspace implementation.
//
//   - The data plane owns everything per-packet: the UDP socket, the Noise
//     handshakes and keypairs, encryption, the tun devices and the
//     dst-peerid -> next-hop link table. It forwards proto=0 (tunneled IP)
//     packets entirely on its own.
//   - Any packet with a non-zero proto in the 4-byte routing header is a
//     control packet. The data plane never interprets it: it punts it up to
//     the control plane (CmdPunt), and sends whatever the control plane hands
//     it (CmdInject).
//   - The control plane puppets the data plane: it configures the device
//     (DeviceSet), adds/removes links, asks for tuns to be created, started and
//     destroyed, and installs/changes/removes next-hop routes.
//   - The data plane tells the control plane about the few things it cannot
//     infer (CmdEvent, currently "a handshake completed"). Events are lossy by
//     design; anything that matters is recoverable with a dump.
//
// Transport (userspace data plane): a unix SOCK_SEQPACKET socket, one netlink
// message per datagram. Message boundaries are preserved, delivery is reliable
// and ordered, and a peer going away is an immediate EOF, which is what makes
// "the control plane died" cheap to notice.
package dpproto

import (
	"errors"
	"fmt"
)

// Version is the API version, carried in every genlmsghdr and in Hello. It is
// bumped on an incompatible change; adding a command or attribute is not one.
const Version = 3

// Commands (genl "ops"). The reply to a request carries the same command.
const (
	CmdHello      uint8 = 1  // doit: describe this data plane
	CmdAttach     uint8 = 2  // doit: become the control plane (punts and events come to this socket)
	CmdDeviceSet  uint8 = 3  // doit: configure the device (once)
	CmdDeviceDel  uint8 = 4  // doit: tear the device down; back to unconfigured
	CmdStatsGet   uint8 = 5  // dump
	CmdLinkAdd    uint8 = 6  // doit
	CmdLinkDel    uint8 = 7  // doit
	CmdLinkGet    uint8 = 8  // dump
	CmdTunCreate  uint8 = 9  // doit
	CmdTunStart   uint8 = 10 // doit
	CmdTunDestroy uint8 = 11 // doit
	CmdTunGet     uint8 = 12 // dump
	CmdRouteSet   uint8 = 13 // doit
	CmdRouteDel   uint8 = 14 // doit
	CmdRouteGet   uint8 = 15 // dump
	CmdPunt       uint8 = 16 // data plane -> control plane, no reply
	CmdInject     uint8 = 17 // control plane -> data plane, no reply
	CmdEvent      uint8 = 18 // data plane -> control plane, no reply

	// Vendor range: commands only the userspace data plane has. A kernel one
	// has no process to ask to exit (it is unloaded), so nothing above may
	// depend on these.
	CmdXQuit uint8 = 128 // doit: exit the process (after DeviceDel semantics)
)

// Attribute types (one flat namespace, the way small genl families do it).
const (
	AttrUnspec       uint16 = 0
	AttrPad          uint16 = 1 // alignment padding before u64 attributes
	AttrAPIVersion   uint16 = 2 // u32
	AttrPID          uint16 = 3 // u32: userspace data plane's pid (absent in a kernel one)
	AttrConfigured   uint16 = 4 // u8: 1 once a DeviceSet has been applied
	AttrLocalID      uint16 = 5 // u32: this node's id
	AttrPrivateKey   uint16 = 6 // 32 bytes
	AttrPublicKey    uint16 = 7 // 32 bytes
	AttrListenPort   uint16 = 8 // u16, host order
	AttrFwmark       uint16 = 9 // u32
	AttrMTU          uint16 = 10
	AttrPeerID       uint16 = 11 // u32: the link (neighbor) or tun destination this is about
	AttrEndpoint     uint16 = 12 // struct sockaddr_in / sockaddr_in6; absent = none
	AttrLastHS       uint16 = 13 // u64: unix nanoseconds of the last handshake, 0 = never
	AttrTxBytes      uint16 = 14 // u64
	AttrRxBytes      uint16 = 15 // u64
	AttrTunName      uint16 = 16 // NUL-terminated string, at most 15 characters
	AttrTunStarted   uint16 = 17 // u8
	AttrRouteDst     uint16 = 18 // u32
	AttrRouteNextHop uint16 = 19 // u32
	AttrStatID       uint16 = 20 // u16
	AttrStatValue    uint16 = 21 // u64
	AttrPktLink      uint16 = 22 // u32: link peerid (ingress for a punt, egress for an inject)
	AttrPktProto     uint16 = 23 // u8
	AttrPktSrc       uint16 = 24 // u8
	AttrPktDst       uint16 = 25 // u8
	AttrPktTTL       uint16 = 26 // u8
	AttrPktData      uint16 = 27 // binary
	AttrEvtKind      uint16 = 28 // u8
	AttrEvtTime      uint16 = 29 // u64: unix nanoseconds
)

// Error codes are errnos, as they are on a netlink socket (the error reply
// carries -errno, plus the text as an extended-ack message).
const (
	CodeInvalid     uint16 = 22 // EINVAL: malformed or nonsensical request
	CodeNotFound    uint16 = 2  // ENOENT
	CodeExists      uint16 = 17 // EEXIST
	CodeInternal    uint16 = 5  // EIO
	CodeUnsupported uint16 = 95 // EOPNOTSUPP: unknown command / version mismatch
	CodeNotReady    uint16 = 19 // ENODEV: no device yet (DeviceSet)
)

// Error is a failure reported by the data plane for one request.
type Error struct {
	Code uint16 // an errno
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("data plane: %s (errno %d)", e.Msg, e.Code) }

// Errorf builds an *Error for a Handler to return.
func Errorf(code uint16, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err is (or wraps) a data-plane Error with this code.
func IsCode(err error, code uint16) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// ---- messages ---------------------------------------------------------------

// Hello opens a session. The data plane answers with HelloReply.
type Hello struct {
	Version uint16
}

// HelloReply describes the data plane the control plane just attached to.
type HelloReply struct {
	Version    uint16
	PID        uint32 // lets the control plane check which binary it is talking to
	Configured bool   // whether a DeviceSet has already been applied
}

// DeviceSet gives the data plane its identity and how it reaches the network.
// It is the data plane's entire configuration: it starts knowing only its
// socket path. Applying it opens the UDP socket and starts the crypto
// workers.
//
// It is configure-once: repeating the same DeviceSet is a no-op (so a
// control plane can safely send it on every attach), but a *different* one
// fails with CodeExists -- the control plane must DeviceDel first, which drops
// every link, tun and route.
type DeviceSet struct {
	LocalID    uint32
	PrivateKey [32]byte
	ListenPort uint16
	Fwmark     uint32 // SO_MARK on the UDP socket; 0 = unset
	MTU        uint32 // of every tun; also bounds control packet size
}

// DeviceSetReply carries what the data plane derived from the DeviceSet.
type DeviceSetReply struct {
	PubKey [32]byte // this node's static public key
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

// TunCreate asks for the tun leading to destination PeerID, with this
// interface name (the control plane owns naming).
type TunCreate struct {
	PeerID uint32
	Name   string
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

// Stat ids. The stats dump is a list of (id, value) pairs so a data plane can
// gain counters without a protocol change: unknown ids are ignored by the
// control plane, and counters a data plane doesn't have are simply absent.
const (
	StatPuntSent      uint16 = 1  // control packets handed up to the control plane
	StatPuntDropped   uint16 = 2  // ... dropped: no control plane attached, or its queue was full
	StatInjectSent    uint16 = 3  // control packets accepted for transmission
	StatInjectDropped uint16 = 4  // ... dropped: unknown/stopped link, oversize, or queues full
	StatRxNoRoute     uint16 = 5  // forwarded packets dropped: no route or no such next-hop link
	StatRxTTLExpired  uint16 = 6  // forwarded packets dropped: TTL ran out
	StatRxNoTun       uint16 = 7  // packets for this node dropped: no started tun for the sender
	StatRxTunFull     uint16 = 8  // packets for this node dropped: tun writer queue full
	StatRxBadPacket   uint16 = 9  // decrypted packets dropped: short header or invalid inner IP
	StatTxNoRoute     uint16 = 10 // outbound packets dropped: no route, link not running, or no session on it yet
	StatTxQueueFull   uint16 = 11 // outbound batches tail-dropped: staged or encryption queue full
	StatEventsDropped uint16 = 12 // events dropped: no control plane attached, or its queue was full
	StatRxQueueFull   uint16 = 13 // received batches tail-dropped: decryption queue full
)

// StatName gives a stat its stable, human-readable name (for introspection).
var StatName = map[uint16]string{
	StatPuntSent:      "punt_sent",
	StatPuntDropped:   "punt_dropped",
	StatInjectSent:    "inject_sent",
	StatInjectDropped: "inject_dropped",
	StatRxNoRoute:     "rx_no_route",
	StatRxTTLExpired:  "rx_ttl_expired",
	StatRxNoTun:       "rx_no_tun",
	StatRxTunFull:     "rx_tun_queue_full",
	StatRxBadPacket:   "rx_bad_packet",
	StatTxNoRoute:     "tx_no_route",
	StatTxQueueFull:   "tx_queue_full",
	StatEventsDropped: "events_dropped",
	StatRxQueueFull:   "rx_queue_full",
}

// Stat is one datapath counter.
type Stat struct {
	ID    uint16
	Value uint64
}

// Event kinds.
const (
	// EventLinkHandshake: a Noise handshake completed on a link. Endpoint is
	// where the peer currently is, so a roamed listen-only link shows up here
	// (at rekey granularity, which is plenty for operations and costs nothing
	// on the packet path).
	EventLinkHandshake uint8 = 1
)

// Event is an asynchronous notification from the data plane. Lossy: when the
// control plane isn't attached or falls behind, events are dropped (and
// counted in StatEventsDropped), never queued without bound. Like netlink's
// ENOBUFS, a control plane that cares about state resyncs with a dump.
type Event struct {
	Kind     uint8
	PeerID   uint32 // link peerid
	UnixNano int64
	Endpoint string
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

// ---- codec: each message is a set of netlink attributes ----------------------------

// The put methods append a message's attributes; the get methods read them
// back, insisting on the ones that are required. Absent optional attributes
// decode to zero values.

func b2u8(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

func (m Hello) put(b *nlb) { b.u32(AttrAPIVersion, uint32(m.Version)) }
func (m *Hello) get(a attrs) error {
	if err := a.need(AttrAPIVersion); err != nil {
		return err
	}
	m.Version = uint16(a.u32(AttrAPIVersion))
	return nil
}

func (m HelloReply) put(b *nlb) {
	b.u32(AttrAPIVersion, uint32(m.Version)).u32(AttrPID, m.PID).u8(AttrConfigured, b2u8(m.Configured))
}
func (m *HelloReply) get(a attrs) error {
	if err := a.need(AttrAPIVersion); err != nil {
		return err
	}
	m.Version = uint16(a.u32(AttrAPIVersion))
	m.PID = a.u32(AttrPID)
	m.Configured = a.u8(AttrConfigured) != 0
	return nil
}

func (m DeviceSet) put(b *nlb) {
	b.u32(AttrLocalID, m.LocalID).attr(AttrPrivateKey, m.PrivateKey[:]).u16(AttrListenPort, m.ListenPort)
	b.u32(AttrFwmark, m.Fwmark).u32(AttrMTU, m.MTU)
}
func (m *DeviceSet) get(a attrs) error {
	if err := a.need(AttrLocalID, AttrPrivateKey, AttrListenPort, AttrMTU); err != nil {
		return err
	}
	m.LocalID = a.u32(AttrLocalID)
	m.PrivateKey = a.key(AttrPrivateKey)
	m.ListenPort = a.u16(AttrListenPort)
	m.Fwmark = a.u32(AttrFwmark)
	m.MTU = a.u32(AttrMTU)
	return nil
}

func (m DeviceSetReply) put(b *nlb) { b.attr(AttrPublicKey, m.PubKey[:]) }
func (m *DeviceSetReply) get(a attrs) error {
	if err := a.need(AttrPublicKey); err != nil {
		return err
	}
	m.PubKey = a.key(AttrPublicKey)
	return nil
}

func putEndpoint(b *nlb, ep string) error {
	if ep == "" {
		return nil
	}
	sa, err := endpointToSockaddr(ep)
	if err != nil {
		return err
	}
	b.attr(AttrEndpoint, sa)
	return nil
}

func getEndpoint(a attrs) (string, error) {
	v, ok := a.get(AttrEndpoint)
	if !ok {
		return "", nil
	}
	return sockaddrToEndpoint(v)
}

func (m LinkAdd) put(b *nlb) error {
	b.u32(AttrPeerID, m.PeerID).attr(AttrPublicKey, m.PubKey[:])
	return putEndpoint(b, m.Endpoint)
}
func (m *LinkAdd) get(a attrs) (err error) {
	if err := a.need(AttrPeerID, AttrPublicKey); err != nil {
		return err
	}
	m.PeerID = a.u32(AttrPeerID)
	m.PubKey = a.key(AttrPublicKey)
	m.Endpoint, err = getEndpoint(a)
	return err
}

func (m LinkInfo) put(b *nlb) error {
	b.u32(AttrPeerID, m.PeerID).attr(AttrPublicKey, m.PubKey[:])
	if err := putEndpoint(b, m.Endpoint); err != nil {
		return err
	}
	b.u64(AttrLastHS, uint64(m.LastHandshakeUnixNano)).u64(AttrTxBytes, m.TxBytes).u64(AttrRxBytes, m.RxBytes)
	return nil
}
func (m *LinkInfo) get(a attrs) (err error) {
	if err := a.need(AttrPeerID, AttrPublicKey); err != nil {
		return err
	}
	m.PeerID = a.u32(AttrPeerID)
	m.PubKey = a.key(AttrPublicKey)
	if m.Endpoint, err = getEndpoint(a); err != nil {
		return err
	}
	m.LastHandshakeUnixNano = int64(a.u64(AttrLastHS))
	m.TxBytes = a.u64(AttrTxBytes)
	m.RxBytes = a.u64(AttrRxBytes)
	return nil
}

// putID / getID are the bodies that name just one id under a given attribute
// (LinkDel, TunStart, TunDestroy: AttrPeerID; RouteDel: AttrRouteDst).
func putID(b *nlb, typ uint16, id uint32) { b.u32(typ, id) }
func getID(a attrs, typ uint16) (uint32, error) {
	if err := a.need(typ); err != nil {
		return 0, err
	}
	return a.u32(typ), nil
}

func (m TunCreate) put(b *nlb) { b.u32(AttrPeerID, m.PeerID).str(AttrTunName, m.Name) }
func (m *TunCreate) get(a attrs) error {
	if err := a.need(AttrPeerID, AttrTunName); err != nil {
		return err
	}
	m.PeerID = a.u32(AttrPeerID)
	m.Name = a.str(AttrTunName)
	return nil
}

func (m TunCreateReply) put(b *nlb) { b.str(AttrTunName, m.Name).u8(AttrTunStarted, b2u8(m.Started)) }
func (m *TunCreateReply) get(a attrs) error {
	if err := a.need(AttrTunName); err != nil {
		return err
	}
	m.Name = a.str(AttrTunName)
	m.Started = a.u8(AttrTunStarted) != 0
	return nil
}

func (m TunInfo) put(b *nlb) {
	b.u32(AttrPeerID, m.PeerID).str(AttrTunName, m.Name).u32(AttrMTU, m.MTU).u8(AttrTunStarted, b2u8(m.Started))
}
func (m *TunInfo) get(a attrs) error {
	if err := a.need(AttrPeerID, AttrTunName); err != nil {
		return err
	}
	m.PeerID = a.u32(AttrPeerID)
	m.Name = a.str(AttrTunName)
	m.MTU = a.u32(AttrMTU)
	m.Started = a.u8(AttrTunStarted) != 0
	return nil
}

func (m Route) put(b *nlb) { b.u32(AttrRouteDst, m.Dst).u32(AttrRouteNextHop, m.NextHop) }
func (m *Route) get(a attrs) error {
	if err := a.need(AttrRouteDst, AttrRouteNextHop); err != nil {
		return err
	}
	m.Dst = a.u32(AttrRouteDst)
	m.NextHop = a.u32(AttrRouteNextHop)
	return nil
}

func (m Stat) put(b *nlb) { b.u16(AttrStatID, m.ID).u64(AttrStatValue, m.Value) }
func (m *Stat) get(a attrs) error {
	if err := a.need(AttrStatID, AttrStatValue); err != nil {
		return err
	}
	m.ID = a.u16(AttrStatID)
	m.Value = a.u64(AttrStatValue)
	return nil
}

func (m Event) put(b *nlb) error {
	b.u8(AttrEvtKind, m.Kind).u32(AttrPeerID, m.PeerID).u64(AttrEvtTime, uint64(m.UnixNano))
	return putEndpoint(b, m.Endpoint)
}
func (m *Event) get(a attrs) (err error) {
	if err := a.need(AttrEvtKind, AttrPeerID); err != nil {
		return err
	}
	m.Kind = a.u8(AttrEvtKind)
	m.PeerID = a.u32(AttrPeerID)
	m.UnixNano = int64(a.u64(AttrEvtTime))
	m.Endpoint, err = getEndpoint(a)
	return err
}

func (m Punt) put(b *nlb) {
	b.u32(AttrPktLink, m.Ingress).u8(AttrPktProto, m.Proto).u8(AttrPktSrc, m.Src).u8(AttrPktDst, m.Dst).u8(AttrPktTTL, m.TTL)
	b.attr(AttrPktData, m.Payload)
}
func (m *Punt) get(a attrs) error {
	if err := a.need(AttrPktLink, AttrPktProto); err != nil {
		return err
	}
	m.Ingress = a.u32(AttrPktLink)
	m.Proto = a.u8(AttrPktProto)
	m.Src = a.u8(AttrPktSrc)
	m.Dst = a.u8(AttrPktDst)
	m.TTL = a.u8(AttrPktTTL)
	m.Payload = a.bin(AttrPktData)
	return nil
}

func (m Inject) put(b *nlb) {
	b.u32(AttrPktLink, m.Link).u8(AttrPktProto, m.Proto).u8(AttrPktDst, m.Dst).u8(AttrPktTTL, m.TTL)
	b.attr(AttrPktData, m.Payload)
}
func (m *Inject) get(a attrs) error {
	if err := a.need(AttrPktLink, AttrPktProto); err != nil {
		return err
	}
	m.Link = a.u32(AttrPktLink)
	m.Proto = a.u8(AttrPktProto)
	m.Dst = a.u8(AttrPktDst)
	m.TTL = a.u8(AttrPktTTL)
	m.Payload = a.bin(AttrPktData)
	return nil
}
