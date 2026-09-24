package main

import (
	"sort"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
)

// reconcile.go: level-triggered sync of the host's tuns and kernel routes
// to what path-vector currently wants.
//
// Path-vector (pathvector.go) owns the *desired* state: pv.best (which dests
// are reachable, via which link) and pv.prefixOwner (which peerid owns each
// advertised prefix). Its event handlers only update that state in memory
// and then Kick() the reconciler; they never create tuns, run hooks or
// touch netlink, so they can't stall the receive goroutines they run on
// (which also process link-liveness packets).
//
// The reconciler is the only thing that touches the host. Each pass it
// compares desired with actual and fixes the difference in a fixed order:
//
//  1. create a tun for every reachable dest that lacks one
//  2. destroy tuns whose dest is no longer reachable
//  3. install/repoint kernel routes for owned prefixes (only once the
//     owner's tun exists), and remove proto-198 routes nobody wants
//
// Because it works from the difference and not from a stream of change
// events, ordering is fixed by construction, a failed step is retried
// (with backoff) instead of being forgotten, and drift caused by anything
// else (a flushed table, a tun that vanished) heals on the next pass.

const (
	reconcileInterval   = 30 * time.Second // safety-net full pass
	reconcileBackoffMin = time.Second
	reconcileBackoffMax = 30 * time.Second

	// rtTableMain is the main routing table. melnode-helper installs its own
	// proto-198 routes in per-peer tables; those must never be touched here.
	rtTableMain = 254
)

// hostOps is everything the reconciler does to the machine. realHost is the
// implementation; tests substitute a fake.
type hostOps interface {
	Tuns() []uint32                           // peerids that currently have a tun
	TunName(peerID uint32) (string, bool)     // interface name of that peer's tun
	EnsureTun(peerID uint32) error            // create (and start) the tun if missing
	DestroyTun(peerID uint32)                 // close and remove the tun
	ListRoutes() (map[pvPrefix]string, error) // our proto-198 main-table routes: prefix -> ifname
	ReplaceRoute(p pvPrefix, ifname string) error
	DelRoute(p pvPrefix) error
}

// reconciler state embedded in Router.
type reconcilerState struct {
	host            hostOps
	desiredPrefixes func() map[pvPrefix]uint32 // set by PathVector
	kickCh          chan struct{}
	reconMu         sync.Mutex // one pass at a time
}

func (s *reconcilerState) init(r *Router) {
	s.host = &realHost{r: r}
	s.kickCh = make(chan struct{}, 1)
}

// Kick asks for a reconcile pass soon. Never blocks; kicks coalesce.
func (r *Router) Kick() {
	select {
	case r.kickCh <- struct{}{}:
	default:
	}
}

// SetRoutes replaces the dst-peerid -> next-hop table (what LookupRoute
// reads on the data path) with routes, wholesale, and kicks the reconciler.
// Callers derive routes from a fresh snapshot of path-vector's state under
// their own lock, so the newest call always carries the newest state.
func (r *Router) SetRoutes(routes map[uint32]uint32) {
	r.mu.Lock()
	old := r.routeTable
	r.routeTable = routes
	r.mu.Unlock()

	for dst, nh := range routes {
		if o, had := old[dst]; !had {
			r.dev.log.Verbosef("router: new route to peerid %d via nhid %d", dst, nh)
		} else if o != nh {
			r.dev.log.Verbosef("router: route to peerid %d updated, now via nhid %d", dst, nh)
		}
	}
	for dst := range old {
		if _, still := routes[dst]; !still {
			r.dev.log.Verbosef("router: route to peerid %d withdrawn", dst)
		}
	}
	r.Kick()
}

// StartReconciler runs the reconcile loop until stop is closed.
func (r *Router) StartReconciler(stop <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(reconcileInterval)
		defer tick.Stop()
		backoff := reconcileBackoffMin
		var retry <-chan time.Time
		r.Kick() // initial pass
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

// reconcileOnce makes one pass and reports whether something couldn't be
// fixed (so the caller should retry after a delay).
func (r *Router) reconcileOnce() (retry bool) {
	r.reconMu.Lock()
	defer r.reconMu.Unlock()
	h := r.host

	r.mu.RLock()
	wantDests := make([]uint32, 0, len(r.routeTable))
	inTable := make(map[uint32]bool, len(r.routeTable))
	for d := range r.routeTable {
		wantDests = append(wantDests, d)
		inTable[d] = true
	}
	r.mu.RUnlock()
	sort.Slice(wantDests, func(i, j int) bool { return wantDests[i] < wantDests[j] })

	// 1. tuns for reachable dests
	haveTun := make(map[uint32]bool)
	for _, id := range h.Tuns() {
		haveTun[id] = true
	}
	for _, d := range wantDests {
		if haveTun[d] {
			continue
		}
		if err := h.EnsureTun(d); err != nil {
			r.dev.log.Errorf("router: can't create tun for peerid %d (will retry): %v", d, err)
			retry = true
		}
	}

	// 2. tuns for dests that are gone
	for id := range haveTun {
		if !inTable[id] {
			h.DestroyTun(id)
		}
	}

	// 3. prefix routes
	var want map[pvPrefix]uint32
	if r.desiredPrefixes != nil {
		want = r.desiredPrefixes()
	}
	have, err := h.ListRoutes()
	if err != nil {
		r.dev.log.Errorf("router: listing kernel routes (will retry): %v", err)
		return true
	}
	for p, owner := range want {
		if owner == r.localID {
			// Locally owned: whatever attached it has its own route. Drop
			// any leftover we installed for a previous remote owner.
			if _, ok := have[p]; ok {
				if err := h.DelRoute(p); err != nil {
					r.dev.log.Errorf("router: removing stale kernel route %v: %v", p, err)
					retry = true
				} else {
					r.dev.log.Verbosef("router: removed stale kernel route for %v (now locally owned)", p)
				}
			}
			continue
		}
		name, ok := h.TunName(owner)
		if !ok {
			retry = true // owner's tun isn't up yet (already reported above)
			continue
		}
		if have[p] == name {
			continue
		}
		if err := h.ReplaceRoute(p, name); err != nil {
			r.dev.log.Errorf("router: failed to install kernel route %v via peerid %d (dev %s): %v", p, owner, name, err)
			retry = true
			continue
		}
		r.dev.log.Verbosef("router: kernel route %v -> peerid %d (dev %s)", p, owner, name)
	}
	for p := range have {
		if _, ok := want[p]; ok {
			continue
		}
		if err := h.DelRoute(p); err != nil {
			r.dev.log.Errorf("router: removing kernel route %v: %v", p, err)
			retry = true
		} else {
			r.dev.log.Verbosef("router: kernel route for %v removed (no longer owned)", p)
		}
	}
	return retry
}

// realHost is hostOps backed by real tuns and netlink.
type realHost struct{ r *Router }

func (h *realHost) Tuns() []uint32 {
	h.r.mu.RLock()
	defer h.r.mu.RUnlock()
	ids := make([]uint32, 0, len(h.r.tunByPeerID))
	for id := range h.r.tunByPeerID {
		ids = append(ids, id)
	}
	return ids
}

func (h *realHost) TunName(peerID uint32) (string, bool) { return h.r.tunNameForPeerID(peerID) }

func (h *realHost) EnsureTun(peerID uint32) error { return h.r.ensureTun(peerID) }

func (h *realHost) DestroyTun(peerID uint32) { h.r.destroyTun(peerID) }

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
		if rt.Dst == nil {
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
	})
}

func (h *realHost) DelRoute(p pvPrefix) error {
	return netlink.RouteDel(&netlink.Route{Dst: p.ipNet(), Protocol: routeProtocolMelnode})
}
