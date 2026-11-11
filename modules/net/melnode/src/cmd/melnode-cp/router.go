package main

import (
	"fmt"
	"net/netip"
	"os"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"melnode/dpproto"
)

type Router struct {
	node    *Node
	localID uint32

	identityPrefix *pvPrefix

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

func newRouter(node *Node, localID uint32, identityPrefix *pvPrefix, createHook, destroyHook string) *Router {
	r := &Router{
		node:               node,
		localID:            localID,
		identityPrefix:     identityPrefix,
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
func nodeNexthop(id uint32) string {
	return netip.AddrFrom16([16]byte{0xfe, 0x80, 14: byte(id >> 8), 15: byte(id)}).String()
}

func (r *Router) configureDevice(dev dpproto.DeviceInfo) error {
	link, err := netlink.LinkByIndex(int(dev.IfIndex))
	if err != nil {
		return fmt.Errorf("looking up %s: %w", dev.Name, err)
	}
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for k, v := range map[string]string{"addr_gen_mode": "1", "router_solicitations": "0", "accept_ra": "0", "dad_transmits": "0"} {
		p := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/%s", dev.Name, k)
		if err := os.WriteFile(p, []byte(v), 0); err != nil {
			keep(fmt.Errorf("setting %s: %w", p, err))
		}
	}
	ll, _ := netlink.ParseIPNet(nodeNexthop(r.localID) + "/64")
	if err := netlink.AddrReplace(link, &netlink.Addr{IPNet: ll, Flags: unix.IFA_F_NODAD}); err != nil {
		keep(fmt.Errorf("assigning %v to %s: %w", ll, dev.Name, err))
	}
	if r.identityPrefix != nil {
		addr := &netlink.Addr{IPNet: r.identityPrefix.ipNet()}
		if err := netlink.AddrReplace(link, addr); err != nil {
			keep(fmt.Errorf("assigning identity %v to %s: %w", *r.identityPrefix, dev.Name, err))
		} else {
			r.node.log.Verbosef("router: assigned identity %v to %q", *r.identityPrefix, dev.Name)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		keep(fmt.Errorf("bringing up %s: %w", dev.Name, err))
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
