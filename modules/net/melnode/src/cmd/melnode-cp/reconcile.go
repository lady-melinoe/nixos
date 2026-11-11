package main

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vishvananda/netlink"

	"melnode/dpproto"
)

const (
	reconcileInterval   = 30 * time.Second
	reconcileBackoffMin = time.Second
	reconcileBackoffMax = 30 * time.Second

	rtTableMain = 254
)

// hostOps is what the router does to the host: the device (one, with a neighbour entry for every
// node that can be reached, which maps the address routes to it go via to its id), the data plane's
// routes and the kernel's.
type hostOps interface {
	Ready() bool
	EnsureDevice() error
	Peers() ([]uint32, error)
	// PeerNexthop is the link-local address routes to a node go via.
	PeerNexthop(peerID uint32) (string, bool)
	EnsurePeer(peerID uint32) error
	DestroyPeer(peerID uint32)
	ProgramRoutes(routes map[uint32]uint32) error
	ListRoutes() (map[pvPrefix]string, error)
	ReplaceRoute(p pvPrefix, nexthop string) error
	DelRoute(p pvPrefix) error
}

const attachHold = 10 * time.Second

type reconcilerState struct {
	host            hostOps
	desiredPrefixes func() map[pvPrefix]uint32
	kickCh          chan struct{}
	reconMu         sync.Mutex
	holdUntil       atomic.Int64
	peerErr         map[uint32]string // why each node last failed to set up, under reconMu
	deviceErr       string            // why the device last failed to set up, under reconMu
}

func (s *reconcilerState) holding() bool { return time.Now().UnixNano() < s.holdUntil.Load() }

func (r *Router) startHold() {
	r.holdUntil.Store(time.Now().Add(attachHold).UnixNano())
	time.AfterFunc(attachHold+50*time.Millisecond, r.Kick)
}

func (s *reconcilerState) init(r *Router) {
	s.host = &realHost{r: r}
	s.kickCh = make(chan struct{}, 1)
	s.peerErr = make(map[uint32]string)
}

func (r *Router) Kick() {
	select {
	case r.kickCh <- struct{}{}:
	default:
	}
}

func (r *Router) SetRoutes(routes map[uint32]uint32) {
	r.mu.Lock()
	old := r.routeTable
	r.routeTable = routes
	r.mu.Unlock()

	for dst, nh := range routes {
		if o, had := old[dst]; !had {
			r.node.log.Verbosef("router: new route to peerid %d via nhid %d", dst, nh)
		} else if o != nh {
			r.node.log.Verbosef("router: route to peerid %d updated, now via nhid %d", dst, nh)
		}
	}
	for dst := range old {
		if _, still := routes[dst]; !still {
			r.node.log.Verbosef("router: route to peerid %d withdrawn", dst)
		}
	}
	r.Kick()
}

func (r *Router) StartReconciler(stop <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(reconcileInterval)
		defer tick.Stop()
		backoff := reconcileBackoffMin
		var retry <-chan time.Time
		r.Kick()
		for {
			select {
			case <-stop:
				return
			case <-r.kickCh:
			case <-tick.C:
			case <-retry:
			}
			retry = nil
			if r.reconcileOnce() {
				retry = time.After(backoff)
				if backoff *= 2; backoff > reconcileBackoffMax {
					backoff = reconcileBackoffMax
				}
			} else {
				backoff = reconcileBackoffMin
			}
		}
	}()
}

func (r *Router) reconcileOnce() (retry bool) {
	r.reconMu.Lock()
	defer r.reconMu.Unlock()
	h := r.host
	if !h.Ready() {
		return false
	}

	r.mu.RLock()
	wantDests := make([]uint32, 0, len(r.routeTable))
	inTable := make(map[uint32]bool, len(r.routeTable))
	for d := range r.routeTable {
		wantDests = append(wantDests, d)
		inTable[d] = true
	}
	r.mu.RUnlock()
	sort.Slice(wantDests, func(i, j int) bool { return wantDests[i] < wantDests[j] })

	if err := h.EnsureDevice(); err != nil {
		if msg := err.Error(); r.deviceErr != msg {
			r.deviceErr = msg
			r.node.log.Errorf("router: setting up the device (will retry): %v", err)
		}
		retry = true
	} else {
		r.deviceErr = ""
	}

	havePeer := make(map[uint32]bool)
	peers, err := h.Peers()
	if err != nil {
		r.node.log.Errorf("router: listing the nodes with a neighbour entry (will retry): %v", err)
		return true
	}
	for _, id := range peers {
		havePeer[id] = true
	}
	for _, d := range wantDests {
		if err := h.EnsurePeer(d); err != nil {
			// Retried with a growing backoff; a cause that keeps failing is logged once.
			if msg := err.Error(); r.peerErr[d] != msg {
				r.peerErr[d] = msg
				r.node.log.Errorf("router: can't set up peerid %d (will retry): %v", d, err)
			}
			retry = true
		} else if _, failed := r.peerErr[d]; failed {
			delete(r.peerErr, d)
			r.node.log.Verbosef("router: peerid %d is set up now", d)
		}
	}

	r.mu.RLock()
	routes := make(map[uint32]uint32, len(r.routeTable))
	for d, nh := range r.routeTable {
		routes[d] = nh
	}
	r.mu.RUnlock()
	hold := r.holding()
	if err := h.ProgramRoutes(routes); err != nil {
		r.node.log.Errorf("router: programming data plane routes (will retry): %v", err)
		retry = true
	}

	for id := range havePeer {
		if !inTable[id] {
			h.DestroyPeer(id)
		}
	}

	var want map[pvPrefix]uint32
	if r.desiredPrefixes != nil {
		want = r.desiredPrefixes()
	}
	have, err := h.ListRoutes()
	if err != nil {
		r.node.log.Errorf("router: listing kernel routes (will retry): %v", err)
		return true
	}
	for p, owner := range want {
		if owner == r.localID {
			if _, ok := have[p]; ok {
				if err := h.DelRoute(p); err != nil {
					r.node.log.Errorf("router: removing stale kernel route %v: %v", p, err)
					retry = true
				} else {
					r.node.log.Verbosef("router: removed stale kernel route for %v (now locally owned)", p)
				}
			}
			continue
		}
		name, ok := h.PeerNexthop(owner)
		if !ok {
			retry = true
			continue
		}
		if have[p] == name {
			continue
		}
		if err := h.ReplaceRoute(p, name); err != nil {
			r.node.log.Errorf("router: failed to install kernel route %v via peerid %d (%s): %v", p, owner, name, err)
			retry = true
			continue
		}
		r.node.log.Verbosef("router: kernel route %v -> peerid %d (via %s)", p, owner, name)
	}
	for p := range have {
		if _, ok := want[p]; ok || hold {
			continue
		}
		if err := h.DelRoute(p); err != nil {
			r.node.log.Errorf("router: removing kernel route %v: %v", p, err)
			retry = true
		} else {
			r.node.log.Verbosef("router: kernel route for %v removed (no longer owned)", p)
		}
	}
	return retry
}

var errNoDataplane = errors.New("no data plane attached")

type realHost struct{ r *Router }

func (h *realHost) client() (dpproto.Datapath, error) {
	if cl := h.r.node.dp(); cl != nil {
		return cl, nil
	}
	return nil, errNoDataplane
}

func (h *realHost) Ready() bool { return h.r.node.dp() != nil }

func (h *realHost) EnsureDevice() error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	dev, err := cl.Device()
	if err != nil {
		return err
	}
	h.r.hostMu.Lock()
	h.r.dev = dev
	done := h.r.devConfigured == dev.IfIndex
	h.r.hostMu.Unlock()
	if done {
		return nil
	}
	if err := h.r.configureDevice(dev); err != nil {
		return err
	}
	h.r.hostMu.Lock()
	h.r.devConfigured = dev.IfIndex
	h.r.hostMu.Unlock()
	return nil
}

func (h *realHost) device() (dpproto.DeviceInfo, error) {
	h.r.hostMu.Lock()
	defer h.r.hostMu.Unlock()
	if h.r.dev.IfIndex == 0 {
		return dpproto.DeviceInfo{}, errors.New("the device is not set up yet")
	}
	return h.r.dev, nil
}

// Peers lists the nodes that have a neighbour entry on the device: the entry's link address is
// the node's id.
func (h *realHost) Peers() ([]uint32, error) {
	dev, err := h.device()
	if err != nil {
		return nil, err
	}
	neighs, err := netlink.NeighList(int(dev.IfIndex), netlink.FAMILY_V6)
	if err != nil {
		return nil, err
	}
	nexthops := make(map[uint32]string, len(neighs))
	ids := make([]uint32, 0, len(neighs))
	for _, n := range neighs {
		if n.State&netlink.NUD_PERMANENT == 0 || len(n.HardwareAddr) != 1 || n.IP.String() != nodeNexthop(uint32(n.HardwareAddr[0])) {
			continue
		}
		id := uint32(n.HardwareAddr[0])
		nexthops[id] = n.IP.String()
		ids = append(ids, id)
	}
	h.r.hostMu.Lock()
	for id := range h.r.hooked {
		nexthops[id] = nodeNexthop(id)
	}
	h.r.nexthops = nexthops
	h.r.hostMu.Unlock()
	return ids, nil
}

func (h *realHost) PeerNexthop(peerID uint32) (string, bool) {
	h.r.hostMu.Lock()
	defer h.r.hostMu.Unlock()
	n, ok := h.r.nexthops[peerID]
	return n, ok
}

func (h *realHost) EnsurePeer(peerID uint32) error {
	h.r.hostMu.Lock()
	_, exists := h.r.nexthops[peerID]
	ready := exists && h.r.hooked[peerID]
	h.r.hostMu.Unlock()
	if ready {
		return nil
	}
	dev, err := h.device()
	if err != nil {
		return err
	}
	gw := nodeNexthop(peerID)
	h.r.hostMu.Lock()
	h.r.nexthops[peerID] = gw
	h.r.hostMu.Unlock()

	if err := h.r.runPeerHook("create", h.r.peerCreateHookBin, peerID, dev.Name, gw); err != nil {
		return fmt.Errorf("setting up node %d: %w", peerID, err)
	}
	h.r.hostMu.Lock()
	h.r.hooked[peerID] = true
	h.r.hostMu.Unlock()
	return nil
}

func (h *realHost) DestroyPeer(peerID uint32) {
	h.r.hostMu.Lock()
	gw, had := h.r.nexthops[peerID]
	delete(h.r.nexthops, peerID)
	delete(h.r.hooked, peerID)
	dev := h.r.dev
	h.r.hostMu.Unlock()

	if had {
		if err := h.r.runPeerHook("destroy", h.r.peerDestroyHookBin, peerID, dev.Name, gw); err != nil {
			h.r.node.log.Errorf("router: %v (peerid %d, %s)", err, peerID, dev.Name)
		}
	}
}

func (h *realHost) ProgramRoutes(want map[uint32]uint32) error {
	cl, err := h.client()
	if err != nil {
		return err
	}
	have, err := cl.RouteList()
	if err != nil {
		return err
	}
	cur := make(map[uint32]uint32, len(have))
	for _, rt := range have {
		cur[rt.Dst] = rt.NextHop
	}
	if len(cur) == len(want) {
		same := true
		for d, nh := range want {
			if old, ok := cur[d]; !ok || old != nh {
				same = false
				break
			}
		}
		if same {
			return nil
		}
	}
	routes := make([]dpproto.Route, 0, len(want))
	for d, nh := range want {
		routes = append(routes, dpproto.Route{Dst: d, NextHop: nh})
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Dst < routes[j].Dst })
	return cl.RouteSet(routes)
}

func (h *realHost) ListRoutes() (map[pvPrefix]string, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Protocol: routeProtocolMelnode, Table: rtTableMain},
		netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	out := make(map[pvPrefix]string, len(routes))
	for _, rt := range routes {
		if rt.Dst == nil || rt.Priority != routePriorityMelnode {
			continue
		}
		p, ok := parsePrefix(rt.Dst.String())
		if !ok {
			continue
		}
		v, ok := rt.Via.(*netlink.Via)
		if !ok || v.AddrFamily != netlink.FAMILY_V6 {
			continue
		}
		out[p] = v.Addr.String()
	}
	return out, nil
}

func (h *realHost) ReplaceRoute(p pvPrefix, nexthop string) error {
	dev, err := h.device()
	if err != nil {
		return err
	}
	gw := net.ParseIP(nexthop)
	if gw == nil || gw.To4() != nil {
		return fmt.Errorf("bad next hop %q", nexthop)
	}
	return netlink.RouteReplace(&netlink.Route{
		LinkIndex: int(dev.IfIndex),
		Dst:       p.ipNet(),
		Via:       &netlink.Via{AddrFamily: netlink.FAMILY_V6, Addr: gw},
		Protocol:  routeProtocolMelnode,
		Priority:  routePriorityMelnode,
	})
}

func (h *realHost) DelRoute(p pvPrefix) error {
	return netlink.RouteDel(&netlink.Route{Dst: p.ipNet(), Protocol: routeProtocolMelnode, Priority: routePriorityMelnode})
}
