package main

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"

	"melnode/dpproto"
)

// defaultMTU is the tun MTU assumed until the data plane reports its own
// (Hello): WireGuard's 1420 minus melnode's 4-byte routing header.
const defaultMTU = 1416

// Node is the control plane's process-wide state: who we are, the configured
// links, path-vector, the reconciler, and -- when there is one -- the session
// to the data plane.
//
// Everything except dpc/pubkey/child is set once at startup and never changes.
type Node struct {
	localID uint32
	log     *Logger

	// device is what we tell the data plane it is (DeviceSet), on every attach.
	device    dpproto.DeviceSet
	tunPrefix string
	// dpCommand is the data plane's argv when we own starting it (config:
	// dataplaneCommand); empty when it is supervised elsewhere.
	dpCommand []string
	// kernelDataplane attaches to the melnode kernel module over generic
	// netlink (dpproto.DialKernel) instead of dialing a socket; see
	// Config.KernelDataplane.
	kernelDataplane bool
	verbose         bool

	router     *Router
	pathVector *PathVector
	links      map[uint32]*Link

	// mtu is the tun MTU we configured the data plane with. It bounds control
	// packet sizes (path-vector splits its updates to fit).
	mtu atomic.Int32

	// dpc is the live data plane session, nil while detached (session.go).
	dpc atomic.Pointer[dpHandle]
	// pubkey is this node's public key, as the data plane derived it from the
	// private key we gave it (nil until the first successful attach).
	pubkey atomic.Pointer[[32]byte]

	// child is closed when the data plane process we most recently started
	// exits (nil if we never started one), so we don't start a second while
	// the first is still coming up. See spawn.go.
	childMu sync.Mutex
	child   chan struct{}

	// introspectMu admits one introspection request at a time to the data
	// plane (see introspectDP); zero value ready.
	introspectMu sync.Mutex
}

// introspectDP returns the data plane for a read-only introspection query,
// plus the func that releases it, or nil when detached or another query is
// already running. Introspection (the control socket's live views, the TCP
// API's snapshot refresh) shares the control plane's session: unbounded
// concurrent queries against an already slow data plane could push a call
// past dpproto.CallTimeout, which drops the session (and with it every
// link). Busy just means answering without the data plane's live details
// this time.
func (n *Node) introspectDP() (dpproto.Datapath, func()) {
	cl := n.dp()
	if cl == nil || !n.introspectMu.TryLock() {
		return nil, func() {}
	}
	return cl, n.introspectMu.Unlock
}

// introspectDPWait is introspectDP for the TCP API's snapshot refresh: it
// waits for a running query instead of giving up. There is only ever one
// refresher, so the gate still admits one data plane query at a time.
func (n *Node) introspectDPWait() (dpproto.Datapath, func()) {
	n.introspectMu.Lock()
	cl := n.dp()
	if cl == nil {
		n.introspectMu.Unlock()
		return nil, func() {}
	}
	return cl, n.introspectMu.Unlock
}

// dp returns the attached data plane, or nil.
// dpHandle boxes the Datapath interface so it can live in an atomic.Pointer.
type dpHandle struct{ dpproto.Datapath }

// dp returns the live data plane session, or nil while detached.
func (n *Node) dp() dpproto.Datapath {
	if h := n.dpc.Load(); h != nil {
		return h.Datapath
	}
	return nil
}

// sortedLinks returns the configured links ordered by peerid.
func (n *Node) sortedLinks() []*Link {
	out := make([]*Link, 0, len(n.links))
	for _, l := range n.links {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// newNode builds the control plane from validated config: the links, the
// router/reconciler, and path-vector. Nothing is started, and nothing talks
// to the data plane yet.
func newNode(cfg *Config, verbose bool) (*Node, error) {
	priv, err := cfg.privateKey()
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	n := &Node{
		localID: uint32(cfg.LocalID),
		log:     NewLogger(LogLevelError, ""),
		links:   make(map[uint32]*Link, len(cfg.Links)),
		device: dpproto.DeviceSet{
			LocalID:    uint32(cfg.LocalID),
			PrivateKey: priv,
			ListenPort: uint16(cfg.LocalPort),
			Fwmark:     uint32(cfg.Fwmark),
			MTU:        uint32(cfg.MTU),
		},
		tunPrefix:       cfg.TunPrefix,
		dpCommand:       cfg.DataplaneCommand,
		kernelDataplane: cfg.KernelDataplane,
		verbose:         verbose,
	}
	if verbose {
		n.log = NewLogger(LogLevelVerbose, "")
	}
	n.mtu.Store(int32(cfg.MTU))

	var identityPrefix *pvPrefix
	if cfg.IdentityPrefix != "" {
		parsed, _ := parsePrefix(cfg.IdentityPrefix) // already validated in Config.validate
		identityPrefix = &parsed
	}
	n.router = newRouter(n, n.localID, identityPrefix, cfg.TunCreateHookBin, cfg.TunDestroyHookBin)

	for _, lc := range cfg.Links {
		pub, _ := parsePubKeyBase64(lc.PeerPubkey) // already validated
		n.links[uint32(lc.PeerID)] = newLink(n, uint32(lc.PeerID), pub, lc.Endpoint, uint32(lc.PrependCount))
	}

	n.pathVector = newPathVector(n, n.localID)
	if identityPrefix != nil {
		n.pathVector.AdvertisePrefix(*identityPrefix)
		log.Printf("identity: always advertising %v (assigned to every tun this node owns)", *identityPrefix)
	}
	return n, nil
}
