package main

import (
	"fmt"
	"net/netip"
	"sync"

	"github.com/vishvananda/netlink"

	"melnode/dpproto"
)

type Router struct {
	node    *Node
	localID uint32

	identityPrefix *pvPrefix
	// nodeAddrs is the address routes to a node go via: the device's neighbour table maps it to
	// the node's id.
	nodeAddrs map[uint32]netip.Addr

	peerCreateHookBin  string
	peerDestroyHookBin string

	mu         sync.RWMutex
	routeTable map[uint32]uint32

	hostMu sync.Mutex
	// dev is the data plane's device; devConfigured is the ifindex the identity address was put on.
	dev           dpproto.DeviceInfo
	devConfigured uint32
	nexthops      map[uint32]string
	hooked        map[uint32]bool

	reconcilerState
}

func newRouter(node *Node, localID uint32, identityPrefix *pvPrefix, nodeAddrs map[uint32]netip.Addr, createHook, destroyHook string) *Router {
	r := &Router{
		node:               node,
		localID:            localID,
		identityPrefix:     identityPrefix,
		nodeAddrs:          nodeAddrs,
		peerCreateHookBin:  createHook,
		peerDestroyHookBin: destroyHook,
		routeTable:         make(map[uint32]uint32),
		nexthops:           make(map[uint32]string),
		hooked:             make(map[uint32]bool),
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

// configureDevice gives the device its identity address and brings it up. It does everything it
// can whatever fails, and returns what failed: the reconciler logs that, and tries again.
func (r *Router) configureDevice(dev dpproto.DeviceInfo) error {
	link, err := netlink.LinkByIndex(int(dev.IfIndex))
	if err != nil {
		return fmt.Errorf("looking up %s: %w", dev.Name, err)
	}
	var first error
	if r.identityPrefix != nil {
		addr := &netlink.Addr{IPNet: r.identityPrefix.ipNet()}
		if err := netlink.AddrReplace(link, addr); err != nil {
			first = fmt.Errorf("assigning identity %v to %s: %w", *r.identityPrefix, dev.Name, err)
		} else {
			r.node.log.Verbosef("router: assigned identity %v to %q", *r.identityPrefix, dev.Name)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil && first == nil {
		first = fmt.Errorf("bringing up %s: %w", dev.Name, err)
	}
	return first
}

func (r *Router) attach() {
	r.hostMu.Lock()
	r.nexthops = make(map[uint32]string)
	r.hooked = make(map[uint32]bool)
	r.devConfigured = 0
	r.hostMu.Unlock()
	r.startHold()
	r.Kick()
}

func (r *Router) detach() {
	r.hostMu.Lock()
	done := make(map[uint32]string, len(r.hooked))
	for id := range r.hooked {
		done[id] = r.nexthops[id]
	}
	name := r.dev.Name
	r.nexthops = make(map[uint32]string)
	r.hooked = make(map[uint32]bool)
	r.hostMu.Unlock()
	for id, gw := range done {
		if err := r.runPeerHook("destroy", r.peerDestroyHookBin, id, name, gw); err != nil {
			r.node.log.Errorf("router: %v (peerid %d, %s)", err, id, name)
		}
	}
}
