package dpproto

import (
	"errors"
	"fmt"
)

const Version = 4

const (
	CmdDeviceSet uint8 = 1
	CmdStatsGet  uint8 = 2
	CmdLinkSet   uint8 = 3
	CmdLinkDel   uint8 = 4
	CmdLinkGet   uint8 = 5
	CmdRouteSet  uint8 = 6
	CmdRouteGet  uint8 = 7
	CmdInject    uint8 = 8
	CmdTunSet    uint8 = 9
	CmdTunDel    uint8 = 10
	CmdTunGet    uint8 = 11
	CmdPunt      uint8 = 12
)

const (
	AttrUnspec       uint16 = 0
	AttrPad          uint16 = 1
	AttrLocalID      uint16 = 2
	AttrPrivateKey   uint16 = 3
	AttrPublicKey    uint16 = 4
	AttrListenPort   uint16 = 5
	AttrFwmark       uint16 = 6
	AttrMTU          uint16 = 7
	AttrPeerID       uint16 = 8
	AttrEndpoint     uint16 = 9
	AttrLastHS       uint16 = 10
	AttrTxBytes      uint16 = 11
	AttrRxBytes      uint16 = 12
	AttrTunName      uint16 = 13
	AttrTunIfindex   uint16 = 14
	AttrRoutes       uint16 = 15
	AttrRoute        uint16 = 16
	AttrRouteDst     uint16 = 17
	AttrRouteNextHop uint16 = 18
	AttrStatID       uint16 = 19
	AttrStatValue    uint16 = 20
	AttrPktLink      uint16 = 21
	AttrPktProto     uint16 = 22
	AttrPktSrc       uint16 = 23
	AttrPktDst       uint16 = 24
	AttrPktTTL       uint16 = 25
	AttrPktData      uint16 = 26
)

const (
	CodeInvalid     uint16 = 22
	CodeNotFound    uint16 = 2
	CodeExists      uint16 = 17
	CodeInternal    uint16 = 5
	CodeUnsupported uint16 = 95
	CodeNotReady    uint16 = 19
	CodeAddrInUse   uint16 = 98
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

type LinkSet struct {
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

type TunSet struct {
	PeerID uint32
	Name   string
}

type TunInfo struct {
	PeerID  uint32
	Name    string
	IfIndex uint32
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
	StatRxQueueFull   uint16 = 12
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
	StatRxQueueFull:   "rx_queue_full",
}

type Stat struct {
	ID    uint16
	Value uint64
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

func (m LinkSet) put(b *nlb) error {
	b.u32(AttrPeerID, m.PeerID).attr(AttrPublicKey, m.PubKey[:])
	return putEndpoint(b, m.Endpoint)
}
func (m *LinkSet) get(a attrs) (err error) {
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

func (m TunSet) put(b *nlb) { b.u32(AttrPeerID, m.PeerID).str(AttrTunName, m.Name) }
func (m *TunSet) get(a attrs) error {
	if err := a.need(AttrPeerID, AttrTunName); err != nil {
		return err
	}
	m.PeerID = a.u32(AttrPeerID)
	m.Name = a.str(AttrTunName)
	return nil
}

func (m TunInfo) put(b *nlb) {
	b.u32(AttrPeerID, m.PeerID).str(AttrTunName, m.Name).u32(AttrTunIfindex, m.IfIndex)
}
func (m *TunInfo) get(a attrs) error {
	if err := a.need(AttrPeerID, AttrTunName); err != nil {
		return err
	}
	m.PeerID = a.u32(AttrPeerID)
	m.Name = a.str(AttrTunName)
	m.IfIndex = a.u32(AttrTunIfindex)
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

func putRoutes(b *nlb, routes []Route) {
	b.nest(AttrRoutes, func(b *nlb) {
		for _, r := range routes {
			b.nest(AttrRoute, r.put)
		}
	})
}

func getRoutes(a attrs) ([]Route, error) {
	raw, ok := a.get(AttrRoutes)
	if !ok {
		return nil, Errorf(CodeInvalid, "missing attribute %d", AttrRoutes)
	}
	entries, err := parseAttrs(raw)
	if err != nil {
		return nil, Errorf(CodeInvalid, "malformed route table: %v", err)
	}
	out := make([]Route, 0, len(entries))
	for _, e := range entries {
		if e.typ != AttrRoute {
			return nil, Errorf(CodeInvalid, "unexpected attribute %d in route table", e.typ)
		}
		ea, err := parseAttrs(e.data)
		if err != nil {
			return nil, Errorf(CodeInvalid, "malformed route entry: %v", err)
		}
		var r Route
		if err := r.get(ea); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
