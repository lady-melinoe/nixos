// Package kmoddp is the data plane on the melinoe-vpn kernel module: dpproto.Datapath over its
// generic netlink family. The netlink socket owns the device: when it closes, however the
// process exits, the kernel deletes the device, and peers see the node go down.
package kmoddp

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"melnode/dpproto"
)

const familyName = "melinoe-vpn"

// Commands and attributes of the family (netlink/specs/melinoe-vpn.yaml in melinoe-vpn-kmod).
const (
	cmdDevNew = iota + 1
	cmdDevGet
	cmdLinkSet
	cmdLinkDel
	cmdLinkGet
	cmdRouteSet
	cmdRouteGet
	cmdProtoSet
	cmdProtoDel
	cmdSend
	cmdLivenessForce
	cmdPunt
	cmdLivenessEvent
)

const (
	devIfindex = iota + 1
	devIfname
	devLocalID
	devPrivateKey
	devPublicKey
	devListenPort
	devFwmark
	devMTU
)

const (
	linkIfindex = iota + 1
	linkID
	linkPublicKey
	linkEndpointAddr
	linkEndpointPort
	linkLastHandshake
	linkRxBytes
	linkTxBytes
	linkLivenessState
	linkLivenessDiag
	linkLivenessSeq
	linkPassive
	linkLocalDisc
	linkRemoteDisc
	linkRemoteTxUs
	linkRemoteRxUs
	linkRttUs
)

const (
	routeDst = iota + 1
	routeNexthop
)

const (
	routesIfindex = iota + 1
	routesRoute
)

const (
	protoIfindex = iota + 1
	protoProto
	protoKind
	protoVersionOffset
	protoVersionMask
	protoVersionShift
	protoVersions
)

const (
	pverVersion = iota + 1
	pverLenOffset
	pverLenSize
	pverLenAdd
	pverMinLen
)

const (
	frameIfindex = iota + 1
	frameLink
	frameProto
	frameSrc
	frameDst
	frameTTL
	framePayload
)

const (
	livIfindex = iota + 1
	livLink
	livState
	livDiag
	livSeq
)

const (
	kindLinkDirect  = 1
	kindForwardable = 2
)

// linkDirectProto is a control proto melnode-cp sends to and receives from adjacent nodes: its
// version is byte 0 of its payload, and each version it speaks says where its length is
// (big-endian, from the start of the payload), so the kernel cuts frames to it and drops
// versions melnode-cp does not speak.
type linkDirectProto struct {
	proto    uint8
	versions []protoVersion
}

type protoVersion struct {
	version, lenSize uint8
	lenOff, minLen   uint16
}

// path-vector version 2: version, flags, then the message's whole length; a 24-byte header.
var linkDirectProtos = []linkDirectProto{{proto: 2, versions: []protoVersion{{version: 2, lenOff: 2, lenSize: 2, minLen: 24}}}}

// Config: callbacks for what the kernel sends up. They run on the reader goroutine and must
// not block.
type Config struct {
	OnPunt     func(dpproto.Punt)
	OnLiveness func(dpproto.LivenessEvent)
	Logf       func(format string, args ...any)
}

type DP struct {
	cfg Config
	c   *conn

	mu      sync.Mutex
	isSet   bool
	devCfg  dpproto.DeviceSet
	pub     [32]byte
	ifindex uint32

	done    chan struct{}
	errOnce sync.Once
	err     error
}

var _ dpproto.Datapath = (*DP)(nil)

// Open connects to the module. Nothing exists in the kernel until DeviceSet.
func Open(cfg Config) (*DP, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	d := &DP{cfg: cfg, done: make(chan struct{})}
	c, err := dial(familyName, d.event)
	if err != nil {
		return nil, fmt.Errorf("kmoddp: %w", err)
	}
	d.c = c
	go func() {
		<-c.readDone
		d.fail(fmt.Errorf("kmoddp: netlink: %v", c.readErr))
	}()
	return d, nil
}

func (d *DP) event(m msg) {
	switch m.cmd {
	case cmdPunt:
		if d.cfg.OnPunt != nil {
			d.cfg.OnPunt(dpproto.Punt{
				Ingress: uint32(m.attrs.u8(frameLink)),
				Proto:   m.attrs.u8(frameProto),
				Src:     m.attrs.u8(frameSrc),
				Dst:     m.attrs.u8(frameDst),
				TTL:     m.attrs.u8(frameTTL),
				Payload: append([]byte(nil), m.attrs.bytes(framePayload)...),
			})
		}
	case cmdLivenessEvent:
		if d.cfg.OnLiveness != nil {
			d.cfg.OnLiveness(dpproto.LivenessEvent{
				Link:  uint32(m.attrs.u8(livLink)),
				State: m.attrs.u8(livState),
				Diag:  m.attrs.u8(livDiag),
				Seq:   m.attrs.u32(livSeq),
			})
		}
	}
}

func (d *DP) Done() <-chan struct{} { return d.done }

func (d *DP) Err() error {
	select {
	case <-d.done:
		return d.err
	default:
		return nil
	}
}

func (d *DP) fail(err error) {
	d.errOnce.Do(func() {
		d.err = err
		close(d.done)
	})
}

// Close closes the socket, and with it the device.
func (d *DP) Close() {
	d.c.close()
	d.fail(errors.New("kmoddp: closed"))
}

func codeFor(err error) uint16 {
	var e *errnoError
	if !errors.As(err, &e) {
		return dpproto.CodeInternal
	}
	switch e.errno {
	case unix.EEXIST:
		return dpproto.CodeExists
	case unix.ENOENT, unix.ENODEV:
		return dpproto.CodeNotFound
	case unix.EINVAL, unix.ERANGE:
		return dpproto.CodeInvalid
	case unix.EADDRINUSE:
		return dpproto.CodeAddrInUse
	case unix.EOPNOTSUPP:
		return dpproto.CodeUnsupported
	}
	return dpproto.CodeInternal
}

func wrap(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	return dpproto.Errorf(codeFor(err), "%s: %v", fmt.Sprintf(format, args...), err)
}

func (d *DP) ready() error {
	if !d.isSet {
		return dpproto.Errorf(dpproto.CodeNotReady, "device not configured")
	}
	return nil
}

// DeviceSet creates the device, owned by this connection, and registers melnode-cp's
// control protos. Called again with the same configuration it returns the public key.
func (d *DP) DeviceSet(m dpproto.DeviceSet) (dpproto.DeviceSetReply, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m.LocalID == 0 || m.LocalID > 255 {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "local id %d out of range 1-255", m.LocalID)
	}
	if d.isSet {
		if m != d.devCfg {
			return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeExists, "already configured differently")
		}
		return dpproto.DeviceSetReply{PubKey: d.pub}, nil
	}
	a := attrs(nil).str(devIfname, m.IfName).u8(devLocalID, uint8(m.LocalID)).
		add(devPrivateKey, m.PrivateKey[:]).u16(devListenPort, m.ListenPort).u32(devFwmark, m.Fwmark)
	if m.MTU != 0 {
		a = a.u32(devMTU, m.MTU)
	}
	var r []msg
	var err error
	// A device a previous owner left is deleted shortly after it exits: wait for the name.
	for i := 0; i < 50; i++ {
		r, err = d.c.do(cmdDevNew, 0, a)
		var e *errnoError
		if !errors.As(err, &e) || e.errno != unix.EEXIST {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return dpproto.DeviceSetReply{}, wrap(err, "creating device %s", m.IfName)
	}
	if len(r) == 0 {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInternal, "creating device %s: no reply", m.IfName)
	}
	d.ifindex = r[0].attrs.u32(devIfindex)
	copy(d.pub[:], r[0].attrs.bytes(devPublicKey))
	for _, p := range linkDirectProtos {
		a := attrs(nil).u32(protoIfindex, d.ifindex).u8(protoProto, p.proto).u8(protoKind, kindLinkDirect).
			u16(protoVersionOffset, 0)
		for _, v := range p.versions {
			a = a.add(protoVersions|unix.NLA_F_NESTED, attrs(nil).u8(pverVersion, v.version).
				u16(pverLenOffset, v.lenOff).u8(pverLenSize, v.lenSize).u16(pverMinLen, v.minLen))
		}
		if _, err := d.c.do(cmdProtoSet, 0, a); err != nil {
			return dpproto.DeviceSetReply{}, wrap(err, "registering proto %d", p.proto)
		}
	}
	d.isSet, d.devCfg = true, m
	return dpproto.DeviceSetReply{PubKey: d.pub}, nil
}

func (d *DP) LinkSet(m dpproto.LinkSet) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	if m.PeerID == 0 || m.PeerID > 255 {
		return dpproto.Errorf(dpproto.CodeInvalid, "peer id %d out of range", m.PeerID)
	}
	a := attrs(nil).u32(linkIfindex, d.ifindex).u8(linkID, uint8(m.PeerID)).add(linkPublicKey, m.PubKey[:])
	if m.Endpoint != "" {
		ap, err := netip.ParseAddrPort(m.Endpoint)
		if err != nil {
			return dpproto.Errorf(dpproto.CodeInvalid, "invalid endpoint %q (must be an ip:port literal): %v", m.Endpoint, err)
		}
		addr := ap.Addr().Unmap()
		a = a.add(linkEndpointAddr, addr.AsSlice()).be16(linkEndpointPort, ap.Port())
	}
	_, err := d.c.do(cmdLinkSet, 0, a)
	return wrap(err, "link %d", m.PeerID)
}

func (d *DP) LinkDel(peerID uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	_, err := d.c.do(cmdLinkDel, 0, attrs(nil).u32(linkIfindex, d.ifindex).u8(linkID, uint8(peerID)))
	return wrap(err, "removing link %d", peerID)
}

func endpointString(p parsed) string {
	port := p.u16be(linkEndpointPort)
	b := p.bytes(linkEndpointAddr)
	switch len(b) {
	case 4:
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b)), port).String()
	case 16:
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(b)), port).String()
	}
	return ""
}

func (d *DP) LinkList() ([]dpproto.LinkInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return nil, err
	}
	r, err := d.c.dump(cmdLinkGet, attrs(nil).u32(linkIfindex, d.ifindex))
	if err != nil {
		return nil, wrap(err, "listing links")
	}
	out := make([]dpproto.LinkInfo, 0, len(r))
	for _, m := range r {
		p := m.attrs
		li := dpproto.LinkInfo{
			PeerID:                uint32(p.u8(linkID)),
			Endpoint:              endpointString(p),
			LastHandshakeUnixNano: int64(p.u64(linkLastHandshake)),
			RxBytes:               p.u64(linkRxBytes),
			TxBytes:               p.u64(linkTxBytes),
			LivenessValid:         true,
			LivenessState:         p.u8(linkLivenessState),
			LivenessDiag:          p.u8(linkLivenessDiag),
			LivenessSeq:           p.u32(linkLivenessSeq),
			Passive:               p.u8(linkPassive) != 0,
			LocalDiscriminator:    p.u32(linkLocalDisc),
			RemoteDiscriminator:   p.u32(linkRemoteDisc),
			RemoteDesiredMinTXUs:  p.u32(linkRemoteTxUs),
			RemoteRequiredMinRXUs: p.u32(linkRemoteRxUs),
			RTTUs:                 p.u32(linkRttUs),
		}
		copy(li.PubKey[:], p.bytes(linkPublicKey))
		out = append(out, li)
	}
	return out, nil
}

func (d *DP) Device() (dpproto.DeviceInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return dpproto.DeviceInfo{}, err
	}
	ifc, err := net.InterfaceByIndex(int(d.ifindex))
	if err != nil {
		return dpproto.DeviceInfo{}, dpproto.Errorf(dpproto.CodeNotFound, "device %d is gone: %v", d.ifindex, err)
	}
	return dpproto.DeviceInfo{Name: ifc.Name, IfIndex: uint32(ifc.Index)}, nil
}

func (d *DP) RouteSet(routes []dpproto.Route) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	a := attrs(nil).u32(routesIfindex, d.ifindex)
	for _, r := range routes {
		if r.Dst == 0 || r.Dst > 255 || r.NextHop == 0 || r.NextHop > 255 {
			return dpproto.Errorf(dpproto.CodeInvalid, "route %d via %d out of range", r.Dst, r.NextHop)
		}
		a = a.add(routesRoute|unix.NLA_F_NESTED, attrs(nil).u8(routeDst, uint8(r.Dst)).u8(routeNexthop, uint8(r.NextHop)))
	}
	_, err := d.c.do(cmdRouteSet, 0, a)
	return wrap(err, "setting routes")
}

func (d *DP) RouteList() ([]dpproto.Route, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return nil, err
	}
	r, err := d.c.do(cmdRouteGet, 0, attrs(nil).u32(routesIfindex, d.ifindex))
	if err != nil {
		return nil, wrap(err, "listing routes")
	}
	var out []dpproto.Route
	for _, m := range r {
		for _, nest := range m.attrs[routesRoute] {
			p := parseAttrs(nest)
			out = append(out, dpproto.Route{Dst: uint32(p.u8(routeDst)), NextHop: uint32(p.u8(routeNexthop))})
		}
	}
	return out, nil
}

// Inject sends a control frame to an adjacent node (a link-direct proto).
func (d *DP) Inject(m dpproto.Inject) error {
	if len(m.Payload) > dpproto.MaxControlPayload {
		return dpproto.Errorf(dpproto.CodeInvalid, "payload of %d bytes is too long", len(m.Payload))
	}
	d.mu.Lock()
	if err := d.ready(); err != nil {
		d.mu.Unlock()
		return err
	}
	ifindex := d.ifindex
	d.mu.Unlock()
	a := attrs(nil).u32(frameIfindex, ifindex).u8(frameLink, uint8(m.Link)).u8(frameProto, m.Proto).
		u8(frameTTL, m.TTL).add(framePayload, m.Payload)
	_, err := d.c.do(cmdSend, 0, a)
	return wrap(err, "inject to link %d", m.Link)
}

func (d *DP) LivenessForce(m dpproto.LivenessForce) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	_, err := d.c.do(cmdLivenessForce, 0, attrs(nil).u32(livIfindex, d.ifindex).u8(livLink, uint8(m.Link)).
		u8(livState, m.State).u8(livDiag, m.Diag))
	return wrap(err, "liveness of link %d", m.Link)
}

// Stats: the module keeps the device's own counters (ip -s link); none of its own yet.
func (d *DP) Stats() ([]dpproto.Stat, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return nil, err
	}
	return nil, nil
}
