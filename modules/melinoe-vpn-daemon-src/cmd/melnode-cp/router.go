package main

import (
	"sync"

	"github.com/vishvananda/netlink"
)

// Router is the control plane's desired forwarding state (which dst peerid
// goes out over which link) plus the machinery that makes the data plane and
// the host match it (reconcile.go).
//
// The data plane holds the *actual* table and does the per-packet lookups;
// this holds what path-vector currently wants. They meet in the reconciler.
type Router struct {
	node    *Node
	localID uint32

	// identityPrefix, if set (config: identityPrefix), is assigned as the
	// local address of every tun this node owns -- see configureTun.
	// Consistent regardless of which specific link/peer a given tun
	// represents, same idea as a router using one stable loopback/router-id
	// address across every point-to-point interface.
	identityPrefix *pvPrefix

	// tunCreateHookBin / tunDestroyHookBin (config: tunCreateHookBin,
	// tunDestroyHookBin), if non-empty, are run as `<bin> <peerid> <ifname>`
	// after a tun is created and configured / after it is deleted. See
	// hooks.go.
	tunCreateHookBin  string
	tunDestroyHookBin string

	mu         sync.RWMutex
	routeTable map[uint32]uint32 // desired: dst peerid -> nhid (a link peerid)

	// Per-data-plane-session bookkeeping for the real host (reconcile.go).
	// hostMu guards both maps.
	hostMu   sync.Mutex
	tunNames map[uint32]string // peerid -> ifname, as of the last time the data plane told us
	hooked   map[uint32]bool   // peerids whose create hook has run since we attached

	reconcilerState // see reconcile.go
}

func newRouter(node *Node, localID uint32, identityPrefix *pvPrefix, tunCreateHookBin, tunDestroyHookBin string) *Router {
	r := &Router{
		node:              node,
		localID:           localID,
		identityPrefix:    identityPrefix,
		tunCreateHookBin:  tunCreateHookBin,
		tunDestroyHookBin: tunDestroyHookBin,
		routeTable:        make(map[uint32]uint32),
		tunNames:          make(map[uint32]string),
		hooked:            make(map[uint32]bool),
	}
	r.reconcilerState.init(r)
	return r
}

// LookupRoute reads the desired next hop for dst.
func (r *Router) LookupRoute(dstPeerID uint32) (nhid uint32, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nhid, ok = r.routeTable[dstPeerID]
	return
}

// configureTun does the host-side setup of a freshly created tun, before the
// data plane is allowed to start moving traffic through it: brings the
// interface up (without which routes pointing at it fail outright with
// ENETDOWN), gives it this node's identity address, and runs the create
// hook.
//
// The identity (config: identityPrefix) goes on every tun the node owns, not
// just one -- e.g. node1 gets 10.99.0.1/32 on both node-2 and node-3.
// `AddrReplace` rather than `AddrAdd`: idempotent if this ever runs twice
// (it does, when adopting tuns after a control plane restart).
func (r *Router) configureTun(peerID uint32, name string) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		r.node.log.Errorf("router: netlink lookup for tun %q failed: %v", name, err)
	} else {
		if err := netlink.LinkSetUp(link); err != nil {
			r.node.log.Errorf("router: failed to bring up tun %q: %v", name, err)
		}
		if r.identityPrefix != nil {
			addr := &netlink.Addr{IPNet: r.identityPrefix.ipNet()}
			if err := netlink.AddrReplace(link, addr); err != nil {
				r.node.log.Errorf("router: failed to assign identity %v to tun %q: %v", *r.identityPrefix, name, err)
			} else {
				r.node.log.Verbosef("router: assigned identity %v to tun %q", *r.identityPrefix, name)
			}
		}
	}
	r.runTunHook("create", r.tunCreateHookBin, peerID, name)
}

// attach is called when a data plane session comes up. Whatever we knew about
// the data plane's tuns is stale (it may even be a different process), so
// forget it, and reconcile from scratch.
func (r *Router) attach() {
	r.hostMu.Lock()
	r.tunNames = make(map[uint32]string)
	r.hooked = make(map[uint32]bool)
	r.hostMu.Unlock()
	r.startHold()
	r.Kick()
}

// detach is called when the data plane session was lost. If the data plane
// process died, its tuns went with it, so undo what the create hooks set up
// for them (the destroy hooks); if it merely reconnects, attach re-runs the
// create hooks. Not called on a deliberate control plane shutdown, which
// leaves the data plane forwarding with its tuns intact.
func (r *Router) detach() {
	r.hostMu.Lock()
	done := make(map[uint32]string, len(r.hooked))
	for id := range r.hooked {
		done[id] = r.tunNames[id]
	}
	r.tunNames = make(map[uint32]string)
	r.hooked = make(map[uint32]bool)
	r.hostMu.Unlock()
	for id, name := range done {
		if name != "" {
			r.runTunHook("destroy", r.tunDestroyHookBin, id, name)
		}
	}
}
