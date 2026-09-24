package main

import (
	"golang.zx2c4.com/wireguard/conn"

	"melnode/dpproto"
)

// ctl.go is the data plane's side of the dpproto socket: it implements
// dpproto.Handler by driving the Device (links) and Router (tuns, routes).
// Nothing in here decides anything -- every mutation is something the
// control plane asked for.

type ctlHandler struct {
	dev  *Device
	port uint16 // UDP port actually bound; reported in Hello
}

var _ dpproto.Handler = (*ctlHandler)(nil)

func (h *ctlHandler) Hello(dpproto.Hello) (dpproto.HelloReply, error) {
	d := h.dev
	d.staticIdentity.RLock()
	pub := d.staticIdentity.publicKey
	d.staticIdentity.RUnlock()
	return dpproto.HelloReply{
		Version: dpproto.Version,
		LocalID: d.localID,
		PubKey:  pub,
		MTU:     uint32(d.mtu),
		Port:    h.port,
	}, nil
}

// LinkAdd creates the Noise tunnel to one neighbor and starts it, or (if the
// link already exists with the same key) re-applies its configured endpoint.
func (h *ctlHandler) LinkAdd(m dpproto.LinkAdd) error {
	d := h.dev
	if m.PeerID > 255 {
		return dpproto.Errorf(dpproto.CodeInvalid, "peerid %d out of range (0-255)", m.PeerID)
	}
	if m.PeerID == d.localID {
		return dpproto.Errorf(dpproto.CodeInvalid, "peerid %d is this node itself", m.PeerID)
	}
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

	role := "listen-only (no endpoint configured)"
	if endpoint != nil {
		role = "dialing " + endpoint.DstToString()
	}
	d.log.Verbosef("link -> peerid %d: %s", m.PeerID, role)
	return nil
}

// LinkDel stops and forgets a link. Routes using it as next hop are the
// control plane's to remove (until it does, they just drop).
func (h *ctlHandler) LinkDel(id uint32) error {
	d := h.dev
	p := d.lookupPeerByID(id)
	if p == nil {
		return dpproto.Errorf(dpproto.CodeNotFound, "no link %d", id)
	}
	p.Stop()
	d.removePeer(p)
	d.log.Verbosef("link peerid %d removed", id)
	return nil
}

func (h *ctlHandler) LinkList() ([]dpproto.LinkInfo, error) {
	d := h.dev
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

func (h *ctlHandler) TunCreate(peerID uint32) (dpproto.TunCreateReply, error) {
	if peerID > 255 {
		return dpproto.TunCreateReply{}, dpproto.Errorf(dpproto.CodeInvalid, "peerid %d out of range (0-255)", peerID)
	}
	if peerID == h.dev.localID {
		return dpproto.TunCreateReply{}, dpproto.Errorf(dpproto.CodeInvalid, "no tun for this node itself (peerid %d)", peerID)
	}
	name, started, err := h.dev.router.CreateTun(peerID)
	if err != nil {
		return dpproto.TunCreateReply{}, err
	}
	return dpproto.TunCreateReply{Name: name, Started: started}, nil
}

func (h *ctlHandler) TunStart(peerID uint32) error { return h.dev.router.StartTun(peerID) }

func (h *ctlHandler) TunDestroy(peerID uint32) error {
	h.dev.router.DestroyTun(peerID)
	return nil
}

func (h *ctlHandler) TunList() ([]dpproto.TunInfo, error) { return h.dev.router.Tuns(), nil }

func (h *ctlHandler) RouteSet(r dpproto.Route) error {
	if r.Dst > 255 || r.NextHop > 255 {
		return dpproto.Errorf(dpproto.CodeInvalid, "route %d via %d: peerids are 0-255", r.Dst, r.NextHop)
	}
	h.dev.router.SetRoute(r.Dst, r.NextHop)
	return nil
}

func (h *ctlHandler) RouteDel(dst uint32) error {
	h.dev.router.DelRoute(dst)
	return nil
}

func (h *ctlHandler) RouteList() ([]dpproto.Route, error) { return h.dev.router.Routes(), nil }

// Inject transmits a control-plane packet on one link. Called from the
// session reader, so it never blocks: staging tail-drops when queues are full
// and the packet is simply lost, which control protocols tolerate by design.
func (h *ctlHandler) Inject(m dpproto.Inject) {
	d := h.dev
	peer := d.lookupPeerByID(m.Link)
	if peer == nil || !peer.isRunning.Load() {
		return
	}
	if len(m.Payload) > d.mtu {
		d.log.Errorf("inject on link %d: %d-byte payload exceeds mtu %d, dropping", m.Link, len(m.Payload), d.mtu)
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
	container.isControl = true // priority class: never queued behind data, see send.go's StagePackets
	container.elems = append(container.elems, elem)

	peer.StagePackets(container)
	peer.SendStagedPackets()
}
