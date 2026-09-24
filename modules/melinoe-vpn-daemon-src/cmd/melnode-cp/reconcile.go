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

// reconcile.go: level-triggered sync of the host's tuns and kernel routes
// to what path-vector currently wants.
//
// Path-vector (pathvector.go) owns the *desired* state: pv.best (which dests
// are reachable, via which link) and pv.prefixOwner (which peerid owns each
// advertised prefix). Its event handlers only update that state in memory
// and then Kick() the reconciler; they never talk to the data plane, run
// hooks or touch netlink, so they can't stall the goroutines they run on
// (which also process link-liveness packets).
//
// The reconciler is the only thing that touches the data plane and the host.
// Each pass it compares desired with actual and fixes the difference in a
// fixed order:
//
//  1. ask the data plane to create (and, once configured, start) a tun for
//     every reachable dest that lacks one
//  2. program the data plane's dst -> next-hop table to match
//  3. ask the data plane to destroy tuns whose dest is no longer reachable
//  4. install/repoint kernel routes for owned prefixes (only once the
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

// hostOps is everything the reconciler does to the data plane and the machine.
// realHost is the implementation; tests substitute a fake.
type hostOps interface {
	Ready() bool                                              // is a data plane attached? (if not, there is nothing to reconcile)
	Tuns() ([]uint32, error)                                  // peerids that currently have a tun
	TunName(peerID uint32) (string, bool)                     // interface name of that peer's tun
	EnsureTun(peerID uint32) error                            // create, configure and start the tun if missing
	DestroyTun(peerID uint32)                                 // close and remove the tun
	ProgramRoutes(routes map[uint32]uint32, prune bool) error // add/repoint the data plane's next hops to match routes; with prune, also remove any others
	ListRoutes() (map[pvPrefix]string, error)                 // our proto-198 main-table routes: prefix -> ifname
	ReplaceRoute(p pvPrefix, ifname string) error
	DelRoute(p pvPrefix) error
}

// attachHold is how long after attaching to a data plane the reconciler
// refrains from *removing* anything it inherited (tuns, next hops, kernel
// prefix routes). On a control plane restart the data plane has been
// forwarding all along, but path-vector starts empty and needs a moment to
// re-learn the mesh; without this, the empty initial state would tear down
// every tun only to recreate it a second later. Additions and repoints are
// never held back. After the hold, anything still not wanted is removed.
const attachHold = 10 * time.Second

// reconciler state embedded in Router.
type reconcilerState struct {
	host            hostOps
	desiredPrefixes func() map[pvPrefix]uint32 // set by PathVector
	kickCh          chan struct{}
	reconMu         sync.Mutex   // one pass at a time
	holdUntil       atomic.Int64 // unix nanos; removals are deferred until then, see attachHold
}

// holding reports whether removals are still being deferred.
func (s *reconcilerState) holding() bool { return time.Now().UnixNano() < s.holdUntil.Load() }

// startHold defers removals for attachHold and arranges a pass when it ends.
func (r *Router) startHold() {
	r.holdUntil.Store(time.Now().Add(attachHold).UnixNano())
	time.AfterFunc(attachHold+50*time.Millisecond, r.Kick)
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
	if !h.Ready() {
		// No data plane attached: nothing to reconcile against. Attaching
		// kicks us (Router.attach), so there's no need to retry on a timer.
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

	// 1. tuns for reachable dests
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
		if haveTun[d] {
			continue
		}
		if err := h.EnsureTun(d); err != nil {
			r.node.log.Errorf("router: can't create tun for peerid %d (will retry): %v", d, err)
			retry = true
		}
	}

	// 2. the data plane's next-hop table
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

	// 3. tuns for dests that are gone
	for id := range haveTun {
		if !inTable[id] && !hold {
			h.DestroyTun(id)
		}
	}

	// 4. prefix routes
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
			// Locally owned: whatever attached it has its own route. Drop
			// any leftover we installed for a previous remote owner.
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
			retry = true // owner's tun isn't up yet (already reported above)
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

// errNoDataplane is returned by realHost operations while no data plane
// session is attached.
var errNoDataplane = errors.New("no data plane attached")

// realHost is hostOps backed by the data plane (over dpproto) and netlink.
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
	ids := make([]uint32, 0, len(tuns))
	for _, t := range tuns {
		names[t.PeerID] = t.Name
		ids = append(ids, t.PeerID)
	}
	h.r.hostMu.Lock()
	h.r.tunNames = names // the data plane is the source of truth for what exists
	h.r.hostMu.Unlock()
	return ids, nil
}

func (h *realHost) TunName(peerID uint32) (string, bool) {
	h.r.hostMu.Lock()
	defer h.r.hostMu.Unlock()
	n, ok := h.r.tunNames[peerID]
	return n, ok
}

// EnsureTun makes dst's tun exist, be configured and carry traffic:
//
//  1. the data plane creates it (idempotent) but leaves it stopped;
//  2. we bring the interface up, give it the node's identity address and run
//     the create hook -- once per data plane session, including for tuns we
//     find already existing (a control plane restart adopts them and re-runs
//     the hook, which is what repopulates state flushed in the meantime,
//     e.g. by an nftables reload);
//  3. only then does the data plane start moving traffic through it, so
//     nothing flows before the interface is fully set up.
func (h *realHost) EnsureTun(peerID uint32) error {
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
		return cl.TunStart(peerID)
	}
	return nil
}

func (h *realHost) DestroyTun(peerID uint32) {
	h.r.hostMu.Lock()
	name, hadName := h.r.tunNames[peerID]
	delete(h.r.tunNames, peerID)
	delete(h.r.hooked, peerID)
	h.r.hostMu.Unlock()

	if cl, err := h.client(); err == nil {
		if err := cl.TunDestroy(peerID); err != nil {
			h.r.node.log.Errorf("router: destroying tun for peerid %d: %v", peerID, err)
			return
		}
	}
	// Runs after the interface is gone, so it can never race the create hook.
	if hadName {
		h.r.runTunHook("destroy", h.r.tunDestroyHookBin, peerID, name)
	}
}

// ProgramRoutes diffs the data plane's next-hop table against want and
// applies the difference: add, repoint (no tun teardown needed) and, if prune,
// remove.
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
