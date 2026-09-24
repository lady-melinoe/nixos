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
	verbose   bool

	router     *Router
	pathVector *PathVector
	links      map[uint32]*Link

	// mtu is the tun MTU we configured the data plane with. It bounds control
	// packet sizes (path-vector splits its updates to fit).
	mtu atomic.Int32

	// dpc is the live data plane session, nil while detached (session.go).
	dpc atomic.Pointer[dpproto.Client]
	// pubkey is this node's public key, as the data plane derived it from the
	// private key we gave it (nil until the first successful attach).
	pubkey atomic.Pointer[[32]byte]

	// child is closed when the data plane process we most recently started
	// exits (nil if we never started one), so we don't start a second while
	// the first is still coming up. See spawn.go.
	childMu sync.Mutex
	child   chan struct{}
}

// dp returns the attached data plane, or nil.
func (n *Node) dp() *dpproto.Client { return n.dpc.Load() }

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
		tunPrefix: cfg.TunPrefix,
		dpCommand: cfg.DataplaneCommand,
		verbose:   verbose,
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
