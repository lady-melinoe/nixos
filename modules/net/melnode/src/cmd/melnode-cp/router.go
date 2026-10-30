package main

import (
	"errors"
	"fmt"
	"sync"

	"github.com/vishvananda/netlink"
)

type Router struct {
	node    *Node
	localID uint32

	identityPrefix *pvPrefix

	tunCreateHookBin  string
	tunDestroyHookBin string

	mu         sync.RWMutex
	routeTable map[uint32]uint32

	hostMu   sync.Mutex
	tunNames map[uint32]string
	hooked   map[uint32]bool

	reconcilerState
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

func (r *Router) LookupRoute(dstPeerID uint32) (nhid uint32, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nhid, ok = r.routeTable[dstPeerID]
	return
}

// configureTun gives a new tun its identity address, runs the create hook and brings it up. It
// does everything it can whatever fails, and returns what did: the reconciler logs that, and tries
// again.
func (r *Router) configureTun(peerID uint32, name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return errors.Join(fmt.Errorf("looking up %s: %w", name, err), r.runTunHook("create", r.tunCreateHookBin, peerID, name))
	}
	var errs []error
	if r.identityPrefix != nil {
		addr := &netlink.Addr{IPNet: r.identityPrefix.ipNet()}
		if err := netlink.AddrReplace(link, addr); err != nil {
			errs = append(errs, fmt.Errorf("assigning identity %v to %s: %w", *r.identityPrefix, name, err))
		} else {
			r.node.log.Verbosef("router: assigned identity %v to tun %q", *r.identityPrefix, name)
		}
	}
	errs = append(errs, r.runTunHook("create", r.tunCreateHookBin, peerID, name))
	if err := netlink.LinkSetUp(link); err != nil {
		errs = append(errs, fmt.Errorf("bringing up %s: %w", name, err))
	}
	return errors.Join(errs...)
}

func (r *Router) attach() {
	r.hostMu.Lock()
	r.tunNames = make(map[uint32]string)
	r.hooked = make(map[uint32]bool)
	r.hostMu.Unlock()
	r.startHold()
	r.Kick()
}

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
			if err := r.runTunHook("destroy", r.tunDestroyHookBin, id, name); err != nil {
				r.node.log.Errorf("router: %v (peerid %d, %s)", err, id, name)
			}
		}
	}
}
