package main

import (
	"errors"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.zx2c4.com/wireguard/conn"

	"melnode/dpproto"
)

type ctlHandler struct {
	sess *dpproto.Session

	mu     sync.Mutex
	dev    *Device
	params dpproto.DeviceSet
	closed bool

	linkMu sync.Mutex
}

var _ dpproto.Handler = (*ctlHandler)(nil)

func (h *ctlHandler) device() (*Device, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dev == nil {
		return nil, dpproto.Errorf(dpproto.CodeNotReady, "data plane not configured yet (send DeviceSet)")
	}
	return h.dev, nil
}

func (h *ctlHandler) current() *Device {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dev
}

func (h *ctlHandler) Close() {
	h.mu.Lock()
	dev := h.dev
	h.dev = nil
	h.closed = true
	h.mu.Unlock()
	if dev != nil {
		dev.Close()
	}
}

func openBind(port uint16) (conn.Bind, []conn.ReceiveFunc, error) {
	deadline := time.Now().Add(restartGrace)
	for {
		bind := conn.NewStdNetBind()
		fns, _, err := bind.Open(port)
		if err == nil {
			return bind, fns, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) || time.Now().After(deadline) {
			if errors.Is(err, syscall.EADDRINUSE) {
				return nil, nil, dpproto.Errorf(dpproto.CodeAddrInUse, "udp port %d is already in use", port)
			}
			return nil, nil, dpproto.Errorf(dpproto.CodeInternal, "binding udp port %d: %v", port, err)
		}
		time.Sleep(restartPoll)
	}
}

func (h *ctlHandler) DeviceSet(m dpproto.DeviceSet) (dpproto.DeviceSetReply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeNotReady, "session closed")
	}
	if h.dev != nil {
		if m != h.params {
			return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeExists, "already configured with different settings (open a new session to change them)")
		}
		return dpproto.DeviceSetReply{PubKey: h.dev.staticIdentity.publicKey}, nil
	}

	switch {
	case m.LocalID > 255:
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "localID must be 0-255, got %d", m.LocalID)
	case m.ListenPort == 0:
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "listenPort must be set")
	case m.MTU < 576 || m.MTU > 65000:
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "mtu must be between 576 and 65000, got %d", m.MTU)
	}
	priv := NoisePrivateKey(m.PrivateKey)
	if priv == zeroKey {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "private key is all zeroes")
	}

	bind, receiveFuncs, err := openBind(m.ListenPort)
	if err != nil {
		return dpproto.DeviceSetReply{}, err
	}
	if m.Fwmark != 0 {
		if err := bind.SetMark(m.Fwmark); err != nil {
			bind.Close()
			return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInternal, "setting fwmark %d on the udp socket: %v", m.Fwmark, err)
		}
	}

	dev := newDevice(m.LocalID, priv)
	dev.mtu = int(m.MTU)
	dev.log = NewLogger(LogLevelError, "")
	dev.net.bind = bind
	dev.router = newRouter(dev, m.LocalID)
	dev.sess = h.sess
	dev.startCryptoWorkers()

	dev.net.stopping.Add(len(receiveFuncs))
	dev.queue.decryption.wg.Add(len(receiveFuncs))
	dev.queue.handshake.wg.Add(len(receiveFuncs))
	for _, fn := range receiveFuncs {
		go dev.RoutineReceiveIncoming(bind.BatchSize(), fn)
	}

	h.dev, h.params = dev, m
	return dpproto.DeviceSetReply{PubKey: dev.staticIdentity.publicKey}, nil
}

func (h *ctlHandler) Stats() ([]dpproto.Stat, error) {
	d, err := h.device()
	if err != nil {
		return nil, err
	}
	s := &d.stats
	return []dpproto.Stat{
		{ID: dpproto.StatPuntSent, Value: h.sess.PuntsSent()},
		{ID: dpproto.StatPuntDropped, Value: h.sess.PuntDropped()},
		{ID: dpproto.StatInjectSent, Value: s.injectSent.Load()},
		{ID: dpproto.StatInjectDropped, Value: s.injectDropped.Load()},
		{ID: dpproto.StatRxNoRoute, Value: s.rxNoRoute.Load()},
		{ID: dpproto.StatRxTTLExpired, Value: s.rxTTL.Load()},
		{ID: dpproto.StatRxNoTun, Value: s.rxNoTun.Load()},
		{ID: dpproto.StatRxTunFull, Value: s.rxTunFull.Load()},
		{ID: dpproto.StatRxBadPacket, Value: s.rxBad.Load()},
		{ID: dpproto.StatTxNoRoute, Value: s.txNoRoute.Load()},
		{ID: dpproto.StatTxQueueFull, Value: s.txQueueFull.Load()},
		{ID: dpproto.StatRxQueueFull, Value: s.rxQueueFull.Load()},
	}, nil
}

func (h *ctlHandler) LinkSet(m dpproto.LinkSet) error {
	d, err := h.device()
	if err != nil {
		return err
	}
	if m.PeerID > 255 {
		return dpproto.Errorf(dpproto.CodeInvalid, "peerid %d out of range (0-255)", m.PeerID)
	}
	if m.PeerID == d.localID {
		return dpproto.Errorf(dpproto.CodeInvalid, "peerid %d is this node itself", m.PeerID)
	}
	h.linkMu.Lock()
	defer h.linkMu.Unlock()
	pk := NoisePublicKey(m.PubKey)

	var endpoint conn.Endpoint
	if m.Endpoint != "" {
		d.net.RLock()
		bind := d.net.bind
		d.net.RUnlock()
		var err error
		endpoint, err = bind.ParseEndpoint(m.Endpoint)
		if err != nil {
			return dpproto.Errorf(dpproto.CodeInvalid, "invalid endpoint %q (must be an ip:port literal): %v", m.Endpoint, err)
		}
	}

	if p := d.lookupPeerByID(m.PeerID); p != nil {
		if p.handshake.remoteStatic != pk {
			return dpproto.Errorf(dpproto.CodeExists, "link %d already exists with a different public key (LinkDel it first)", m.PeerID)
		}
		if endpoint != nil {
			p.endpoint.Lock()
			p.endpoint.val = endpoint
			p.endpoint.Unlock()
		}
		return nil
	}
	if other := d.LookupPeer(pk); other != nil {
		return dpproto.Errorf(dpproto.CodeExists, "public key already used by link %d", other.id)
	}

	p, err := newPeer(d, m.PeerID, pk, endpoint)
	if err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "setting up link %d: %v", m.PeerID, err)
	}
	d.addPeer(p)
	p.Start()

	return nil
}

func (h *ctlHandler) LinkDel(id uint32) error {
	d, err := h.device()
	if err != nil {
		return err
	}
	h.linkMu.Lock()
	defer h.linkMu.Unlock()
	p := d.lookupPeerByID(id)
	if p == nil {
		return dpproto.Errorf(dpproto.CodeNotFound, "no link %d", id)
	}
	p.Stop()
	d.removePeer(p)
	return nil
}

func (h *ctlHandler) LinkList() ([]dpproto.LinkInfo, error) {
	d, err := h.device()
	if err != nil {
		return nil, err
	}
	d.peers.RLock()
	peers := make([]*Peer, 0, len(d.peers.keyMap))
	for _, p := range d.peers.keyMap {
		peers = append(peers, p)
	}
	d.peers.RUnlock()

	out := make([]dpproto.LinkInfo, 0, len(peers))
	for _, p := range peers {
		li := dpproto.LinkInfo{
			PeerID:                p.id,
			PubKey:                p.handshake.remoteStatic,
			LastHandshakeUnixNano: p.lastHandshakeNano.Load(),
			TxBytes:               p.txBytes.Load(),
			RxBytes:               p.rxBytes.Load(),
		}
		p.endpoint.Lock()
		if p.endpoint.val != nil {
			li.Endpoint = p.endpoint.val.DstToString()
		}
		p.endpoint.Unlock()
		out = append(out, li)
	}
	return out, nil
}

func validIfName(n string) bool {
	return n != "" && len(n) <= 15 && !strings.ContainsAny(n, "/ \t\n")
}

func (h *ctlHandler) TunSet(m dpproto.TunSet) (dpproto.TunInfo, error) {
	d, err := h.device()
	if err != nil {
		return dpproto.TunInfo{}, err
	}
	if m.PeerID > 255 {
		return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeInvalid, "peerid %d out of range (0-255)", m.PeerID)
	}
	if m.PeerID == d.localID {
		return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeInvalid, "no tun for this node itself (peerid %d)", m.PeerID)
	}
	if !validIfName(m.Name) {
		return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeInvalid, "invalid interface name %q", m.Name)
	}
	return d.router.CreateTun(m.PeerID, m.Name)
}

func (h *ctlHandler) TunDel(peerID uint32) error {
	d, err := h.device()
	if err != nil {
		return err
	}
	d.router.DestroyTun(peerID)
	return nil
}

func (h *ctlHandler) TunList() ([]dpproto.TunInfo, error) {
	d, err := h.device()
	if err != nil {
		return nil, err
	}
	return d.router.Tuns(), nil
}

func (h *ctlHandler) RouteSet(routes []dpproto.Route) error {
	d, err := h.device()
	if err != nil {
		return err
	}
	return d.router.ReplaceRoutes(routes)
}

func (h *ctlHandler) RouteList() ([]dpproto.Route, error) {
	d, err := h.device()
	if err != nil {
		return nil, err
	}
	return d.router.Routes(), nil
}

func (h *ctlHandler) Inject(m dpproto.Inject) {
	d := h.current()
	if d == nil {
		return
	}
	peer := d.lookupPeerByID(m.Link)
	if peer == nil || !peer.isRunning.Load() {
		d.stats.injectDropped.Add(1)
		return
	}
	if len(m.Payload) > d.mtu {
		d.stats.injectDropped.Add(1)
		d.log.Errorf("inject on link %d: %d-byte payload exceeds mtu %d, dropping", m.Link, len(m.Payload), d.mtu)
		return
	}
	if peer.sendKeypair() == nil {
		peer.SendHandshakeInitiation(false)
		d.stats.injectDropped.Add(1)
		return
	}

	elem := d.NewOutboundElement()
	buf := elem.buffer[:]
	offset := MessageTransportHeaderSize + headerSize
	copy(buf[offset:offset+len(m.Payload)], m.Payload)
	buf[offset-headerSize+0] = m.Proto
	buf[offset-headerSize+1] = byte(d.localID)
	buf[offset-headerSize+2] = m.Dst
	buf[offset-headerSize+hdrOffTTL] = m.TTL
	elem.packet = buf[offset-headerSize : offset+len(m.Payload)]

	container := d.GetOutboundElementsContainer()
	container.isControl = true
	container.elems = append(container.elems, elem)

	d.stats.injectSent.Add(1)
	peer.StagePackets(container)
	peer.SendStagedPackets()
}
