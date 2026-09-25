package main

import (
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
	started  map[uint32]bool

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
		started:           make(map[uint32]bool),
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

func (r *Router) attach() {
	r.hostMu.Lock()
	r.tunNames = make(map[uint32]string)
	r.hooked = make(map[uint32]bool)
	r.started = make(map[uint32]bool)
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
	r.started = make(map[uint32]bool)
	r.hostMu.Unlock()
	for id, name := range done {
		if name != "" {
			r.runTunHook("destroy", r.tunDestroyHookBin, id, name)
		}
	}
}
