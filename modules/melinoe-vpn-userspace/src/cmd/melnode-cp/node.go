package main

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"

	"melnode/dpproto"
)

const defaultMTU = 1416

type Node struct {
	localID uint32
	log     *Logger

	device          dpproto.DeviceSet
	tunPrefix       string
	dpCommand       []string
	kernelDataplane bool
	verbose         bool

	router     *Router
	pathVector *PathVector
	links      map[uint32]*Link

	mtu atomic.Int32

	dpc    atomic.Pointer[dpHandle]
	pubkey atomic.Pointer[[32]byte]

	childMu sync.Mutex
	child   chan struct{}

	introspectMu sync.Mutex
}

func (n *Node) introspectDP() (dpproto.Datapath, func()) {
	cl := n.dp()
	if cl == nil || !n.introspectMu.TryLock() {
		return nil, func() {}
	}
	return cl, n.introspectMu.Unlock
}

func (n *Node) introspectDPWait() (dpproto.Datapath, func()) {
	n.introspectMu.Lock()
	cl := n.dp()
	if cl == nil {
		n.introspectMu.Unlock()
		return nil, func() {}
	}
	return cl, n.introspectMu.Unlock
}

type dpHandle struct{ dpproto.Datapath }

func (n *Node) dp() dpproto.Datapath {
	if h := n.dpc.Load(); h != nil {
		return h.Datapath
	}
	return nil
}

func (n *Node) sortedLinks() []*Link {
	out := make([]*Link, 0, len(n.links))
	for _, l := range n.links {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

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
		parsed, _ := parsePrefix(cfg.IdentityPrefix)
		identityPrefix = &parsed
	}
	n.router = newRouter(n, n.localID, identityPrefix, cfg.TunCreateHookBin, cfg.TunDestroyHookBin)

	for _, lc := range cfg.Links {
		pub, _ := parsePubKeyBase64(lc.PeerPubkey)
		n.links[uint32(lc.PeerID)] = newLink(n, uint32(lc.PeerID), pub, lc.Endpoint, uint32(lc.PrependCount))
	}

	n.pathVector = newPathVector(n, n.localID)
	if identityPrefix != nil {
		n.pathVector.AdvertisePrefix(*identityPrefix)
		log.Printf("identity: always advertising %v (assigned to every tun this node owns)", *identityPrefix)
	}
	return n, nil
}
