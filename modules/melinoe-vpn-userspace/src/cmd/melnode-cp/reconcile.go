package main

import (
	"errors"
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

type hostOps interface {
	Ready() bool
	Tuns() ([]uint32, error)
	TunName(peerID uint32) (string, bool)
	EnsureTun(peerID uint32) error
	DestroyTun(peerID uint32)
	ProgramRoutes(routes map[uint32]uint32, prune bool) error
	ListRoutes() (map[pvPrefix]string, error)
	ReplaceRoute(p pvPrefix, ifname string) error
	DelRoute(p pvPrefix) error
}

const attachHold = 10 * time.Second

type reconcilerState struct {
	host            hostOps
	desiredPrefixes func() map[pvPrefix]uint32
	kickCh          chan struct{}
	reconMu         sync.Mutex
	holdUntil       atomic.Int64
}

func (s *reconcilerState) holding() bool { return time.Now().UnixNano() < s.holdUntil.Load() }

func (r *Router) startHold() {
	r.holdUntil.Store(time.Now().Add(attachHold).UnixNano())
	time.AfterFunc(attachHold+50*time.Millisecond, r.Kick)
}

func (s *reconcilerState) init(r *Router) {
	s.host = &realHost{r: r}
	s.kickCh = make(chan struct{}, 1)
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

	haveTun := make(map[uint32]bool)
	tuns, err := h.Tuns()
	if err != nil {
		r.node.log.Errorf("router: listing data plane tuns (will retry): %v", err)
		return true
	}
	for _, id := range tuns {
		haveTun[id] = true
	}
	for _, d := range wantDests {
		if err := h.EnsureTun(d); err != nil {
			r.node.log.Errorf("router: can't create tun for peerid %d (will retry): %v", d, err)
			retry = true
		}
	}

	r.mu.RLock()
	routes := make(map[uint32]uint32, len(r.routeTable))
	for d, nh := range r.routeTable {
		routes[d] = nh
	}
	r.mu.RUnlock()
	hold := r.holding()
	if err := h.ProgramRoutes(routes, !hold); err != nil {
		r.node.log.Errorf("router: programming data plane routes (will retry): %v", err)
		retry = true
	}

	for id := range haveTun {
		if !inTable[id] && !hold {
			h.DestroyTun(id)
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
		name, ok := h.TunName(owner)
		if !ok {
			retry = true
			continue
		}
		if have[p] == name {
			continue
		}
		if err := h.ReplaceRoute(p, name); err != nil {
			r.node.log.Errorf("router: failed to install kernel route %v via peerid %d (dev %s): %v", p, owner, name, err)
			retry = true
			continue
		}
		r.node.log.Verbosef("router: kernel route %v -> peerid %d (dev %s)", p, owner, name)
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

func (h *realHost) Tuns() ([]uint32, error) {
	cl, err := h.client()
	if err != nil {
		return nil, err
	}
	tuns, err := cl.TunList()
	if err != nil {
		return nil, err
	}
	names := make(map[uint32]string, len(tuns))
	started := make(map[uint32]bool, len(tuns))
	ids := make([]uint32, 0, len(tuns))
	for _, t := range tuns {
		names[t.PeerID] = t.Name
		started[t.PeerID] = t.Started
		ids = append(ids, t.PeerID)
	}
	h.r.hostMu.Lock()
	h.r.tunNames = names
	h.r.started = started
	h.r.hostMu.Unlock()
	return ids, nil
}

func (h *realHost) TunName(peerID uint32) (string, bool) {
	h.r.hostMu.Lock()
	defer h.r.hostMu.Unlock()
	n, ok := h.r.tunNames[peerID]
	return n, ok
}

func (h *realHost) EnsureTun(peerID uint32) error {
	h.r.hostMu.Lock()
	_, exists := h.r.tunNames[peerID]
	ready := exists && h.r.hooked[peerID] && h.r.started[peerID]
	h.r.hostMu.Unlock()
	if ready {
		return nil
	}

	cl, err := h.client()
	if err != nil {
		return err
	}
	rep, err := cl.TunCreate(peerID, h.r.node.tunPrefix+itoa(peerID))
	if err != nil {
		return err
	}
	h.r.hostMu.Lock()
	h.r.tunNames[peerID] = rep.Name
	done := h.r.hooked[peerID]
	h.r.hostMu.Unlock()

	if !done {
		h.r.configureTun(peerID, rep.Name)
		h.r.hostMu.Lock()
		h.r.hooked[peerID] = true
		h.r.hostMu.Unlock()
	}
	if !rep.Started {
		if err := cl.TunStart(peerID); err != nil {
			return err
		}
	}
	h.r.hostMu.Lock()
	h.r.started[peerID] = true
	h.r.hostMu.Unlock()
	return nil
}

func (h *realHost) DestroyTun(peerID uint32) {
	h.r.hostMu.Lock()
	name, hadName := h.r.tunNames[peerID]
	delete(h.r.tunNames, peerID)
	delete(h.r.hooked, peerID)
	delete(h.r.started, peerID)
	h.r.hostMu.Unlock()

	if cl, err := h.client(); err == nil {
		if err := cl.TunDestroy(peerID); err != nil {
			h.r.node.log.Errorf("router: destroying tun for peerid %d: %v", peerID, err)
			return
		}
	}
	if hadName {
		h.r.runTunHook("destroy", h.r.tunDestroyHookBin, peerID, name)
	}
}

func (h *realHost) ProgramRoutes(want map[uint32]uint32, prune bool) error {
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
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for d, nh := range want {
		if old, ok := cur[d]; !ok || old != nh {
			note(cl.RouteSet(dpproto.Route{Dst: d, NextHop: nh}))
		}
	}
	if prune {
		for d := range cur {
			if _, ok := want[d]; !ok {
				note(cl.RouteDel(d))
			}
		}
	}
	return firstErr
}

func (h *realHost) ListRoutes() (map[pvPrefix]string, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Protocol: routeProtocolMelnode, Table: rtTableMain},
		netlink.RT_FILTER_PROTOCOL|netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, err
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	names := make(map[int]string, len(links))
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
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
		out[p] = names[rt.LinkIndex]
	}
	return out, nil
}

func (h *realHost) ReplaceRoute(p pvPrefix, ifname string) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return err
	}
	return netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       p.ipNet(),
		Protocol:  routeProtocolMelnode,
		Priority:  routePriorityMelnode,
	})
}

func (h *realHost) DelRoute(p pvPrefix) error {
	return netlink.RouteDel(&netlink.Route{Dst: p.ipNet(), Protocol: routeProtocolMelnode, Priority: routePriorityMelnode})
}
