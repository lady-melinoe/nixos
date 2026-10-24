// Package dpproto is the contract between the control plane and a data plane: the operations a
// data plane offers, and the types they carry. The only data plane now is the eBPF one in
// package ebpfdp.
package dpproto

import (
	"errors"
	"fmt"
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
