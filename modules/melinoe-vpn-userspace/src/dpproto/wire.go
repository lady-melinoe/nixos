package dpproto

import (
	"errors"
	"fmt"
)

const Version = 3

const (
	CmdHello      uint8 = 1
	CmdAttach     uint8 = 2
	CmdDeviceSet  uint8 = 3
	CmdDeviceDel  uint8 = 4
	CmdStatsGet   uint8 = 5
	CmdLinkAdd    uint8 = 6
	CmdLinkDel    uint8 = 7
	CmdLinkGet    uint8 = 8
	CmdTunCreate  uint8 = 9
	CmdTunStart   uint8 = 10
	CmdTunDestroy uint8 = 11
	CmdTunGet     uint8 = 12
	CmdRouteSet   uint8 = 13
	CmdRouteDel   uint8 = 14
	CmdRouteGet   uint8 = 15
	CmdPunt       uint8 = 16
	CmdInject     uint8 = 17
	CmdEvent      uint8 = 18

	CmdXQuit uint8 = 128
)

const (
	AttrUnspec       uint16 = 0
	AttrPad          uint16 = 1
	AttrAPIVersion   uint16 = 2
	AttrPID          uint16 = 3
	AttrConfigured   uint16 = 4
	AttrLocalID      uint16 = 5
	AttrPrivateKey   uint16 = 6
	AttrPublicKey    uint16 = 7
	AttrListenPort   uint16 = 8
	AttrFwmark       uint16 = 9
	AttrMTU          uint16 = 10
	AttrPeerID       uint16 = 11
	AttrEndpoint     uint16 = 12
	AttrLastHS       uint16 = 13
	AttrTxBytes      uint16 = 14
	AttrRxBytes      uint16 = 15
	AttrTunName      uint16 = 16
	AttrTunStarted   uint16 = 17
	AttrRouteDst     uint16 = 18
	AttrRouteNextHop uint16 = 19
	AttrStatID       uint16 = 20
	AttrStatValue    uint16 = 21
	AttrPktLink      uint16 = 22
	AttrPktProto     uint16 = 23
	AttrPktSrc       uint16 = 24
	AttrPktDst       uint16 = 25
	AttrPktTTL       uint16 = 26
	AttrPktData      uint16 = 27
	AttrEvtKind      uint16 = 28
	AttrEvtTime      uint16 = 29
)

const (
	CodeInvalid     uint16 = 22
	CodeNotFound    uint16 = 2
	CodeExists      uint16 = 17
	CodeInternal    uint16 = 5
	CodeUnsupported uint16 = 95
	CodeNotReady    uint16 = 19
)

type Error struct {
	Code uint16
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("data plane: %s (errno %d)", e.Msg, e.Code) }

func Errorf(code uint16, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

func IsCode(err error, code uint16) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

type Hello struct {
	Version uint16
}

type HelloReply struct {
	Version    uint16
	PID        uint32
	Configured bool
}

type DeviceSet struct {
	LocalID    uint32
	PrivateKey [32]byte
	ListenPort uint16
	Fwmark     uint32
	MTU        uint32
}

type DeviceSetReply struct {
	PubKey [32]byte
}

type LinkAdd struct {
	PeerID   uint32
	PubKey   [32]byte
	Endpoint string
}

type LinkInfo struct {
	PeerID                uint32
	PubKey                [32]byte
	Endpoint              string
	LastHandshakeUnixNano int64
	TxBytes               uint64
	RxBytes               uint64
}

type TunCreate struct {
	PeerID uint32
	Name   string
}

type TunCreateReply struct {
	Name    string
	Started bool
}

type TunInfo struct {
	PeerID  uint32
	Name    string
	MTU     uint32
	Started bool
}

type Route struct {
	Dst     uint32
	NextHop uint32
}

const (
	StatPuntSent      uint16 = 1
	StatPuntDropped   uint16 = 2
	StatInjectSent    uint16 = 3
	StatInjectDropped uint16 = 4
	StatRxNoRoute     uint16 = 5
	StatRxTTLExpired  uint16 = 6
	StatRxNoTun       uint16 = 7
	StatRxTunFull     uint16 = 8
	StatRxBadPacket   uint16 = 9
	StatTxNoRoute     uint16 = 10
	StatTxQueueFull   uint16 = 11
	StatEventsDropped uint16 = 12
	StatRxQueueFull   uint16 = 13
)

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

type Stat struct {
	ID    uint16
	Value uint64
}

const (
	EventLinkHandshake uint8 = 1
)

type Event struct {
	Kind     uint8
	PeerID   uint32
	UnixNano int64
	Endpoint string
}

type Punt struct {
	Ingress uint32
	Proto   uint8
	Src     uint8
	Dst     uint8
	TTL     uint8
	Payload []byte
}

type Inject struct {
	Link    uint32
	Proto   uint8
	Dst     uint8
	TTL     uint8
	Payload []byte
}

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
