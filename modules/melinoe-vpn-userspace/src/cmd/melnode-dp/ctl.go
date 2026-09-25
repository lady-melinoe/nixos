package main

import (
	"os"
	"strings"
	"sync"

	"golang.zx2c4.com/wireguard/conn"

	"melnode/dpproto"
)

type ctlHandler struct {
	srv  *dpproto.Server
	quit func()

	mu     sync.Mutex
	dev    *Device
	params dpproto.DeviceSet

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

func (h *ctlHandler) Hello(dpproto.Hello) (dpproto.HelloReply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return dpproto.HelloReply{Version: dpproto.Version, PID: uint32(os.Getpid()), Configured: h.dev != nil}, nil
}

func (h *ctlHandler) DeviceSet(m dpproto.DeviceSet) (dpproto.DeviceSetReply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.dev != nil {
		if m != h.params {
			return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeExists, "already configured with different settings (restart the data plane to change them)")
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

	bind := conn.NewStdNetBind()
	receiveFuncs, _, err := bind.Open(m.ListenPort)
	if err != nil {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInternal, "binding udp port %d: %v", m.ListenPort, err)
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
	dev.ctl = h.srv
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
	out := []dpproto.Stat{
		{ID: dpproto.StatPuntSent, Value: h.srv.PuntsSent()},
		{ID: dpproto.StatPuntDropped, Value: h.srv.PuntDropped()},
		{ID: dpproto.StatEventsDropped, Value: h.srv.EventsDropped()},
	}
	if d := h.current(); d != nil {
		s := &d.stats
		out = append(out,
			dpproto.Stat{ID: dpproto.StatInjectSent, Value: s.injectSent.Load()},
			dpproto.Stat{ID: dpproto.StatInjectDropped, Value: s.injectDropped.Load()},
			dpproto.Stat{ID: dpproto.StatRxNoRoute, Value: s.rxNoRoute.Load()},
			dpproto.Stat{ID: dpproto.StatRxTTLExpired, Value: s.rxTTL.Load()},
			dpproto.Stat{ID: dpproto.StatRxNoTun, Value: s.rxNoTun.Load()},
			dpproto.Stat{ID: dpproto.StatRxTunFull, Value: s.rxTunFull.Load()},
			dpproto.Stat{ID: dpproto.StatRxBadPacket, Value: s.rxBad.Load()},
			dpproto.Stat{ID: dpproto.StatRxQueueFull, Value: s.rxQueueFull.Load()},
			dpproto.Stat{ID: dpproto.StatTxNoRoute, Value: s.txNoRoute.Load()},
			dpproto.Stat{ID: dpproto.StatTxQueueFull, Value: s.txQueueFull.Load()},
		)
	}
	return out, nil
}

func (h *ctlHandler) DeviceDel() error {
	h.mu.Lock()
	dev := h.dev
	h.dev, h.params = nil, dpproto.DeviceSet{}
	h.mu.Unlock()
	if dev != nil {
		dev.keepCtl = true
		dev.Close()
	}
	return nil
}

func (h *ctlHandler) Quit() { h.quit() }

func (h *ctlHandler) LinkAdd(m dpproto.LinkAdd) error {
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

func (h *ctlHandler) TunCreate(m dpproto.TunCreate) (dpproto.TunCreateReply, error) {
	d, err := h.device()
	if err != nil {
		return dpproto.TunCreateReply{}, err
	}
	if m.PeerID > 255 {
		return dpproto.TunCreateReply{}, dpproto.Errorf(dpproto.CodeInvalid, "peerid %d out of range (0-255)", m.PeerID)
	}
	if m.PeerID == d.localID {
		return dpproto.TunCreateReply{}, dpproto.Errorf(dpproto.CodeInvalid, "no tun for this node itself (peerid %d)", m.PeerID)
	}
	if !validIfName(m.Name) {
		return dpproto.TunCreateReply{}, dpproto.Errorf(dpproto.CodeInvalid, "invalid interface name %q", m.Name)
	}
	name, started, err := d.router.CreateTun(m.PeerID, m.Name)
	if err != nil {
		return dpproto.TunCreateReply{}, err
	}
	return dpproto.TunCreateReply{Name: name, Started: started}, nil
}

func (h *ctlHandler) TunStart(peerID uint32) error {
	d, err := h.device()
	if err != nil {
		return err
	}
	return d.router.StartTun(peerID)
}

func (h *ctlHandler) TunDestroy(peerID uint32) error {
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

func (h *ctlHandler) RouteSet(r dpproto.Route) error {
	d, err := h.device()
	if err != nil {
		return err
	}
	if r.Dst > 255 || r.NextHop > 255 {
		return dpproto.Errorf(dpproto.CodeInvalid, "route %d via %d: peerids are 0-255", r.Dst, r.NextHop)
	}
	d.router.SetRoute(r.Dst, r.NextHop)
	return nil
}

func (h *ctlHandler) RouteDel(dst uint32) error {
	d, err := h.device()
	if err != nil {
		return err
	}
	d.router.DelRoute(dst)
	return nil
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
