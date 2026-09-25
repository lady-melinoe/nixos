package main

import (
	"github.com/vishvananda/netlink"
)

// routeProtocolMelnode tags every kernel route melnode installs
// (`ip route show proto 198`), so they're easy to tell apart from anything
// else, and so removal only ever touches routes we installed.
const routeProtocolMelnode netlink.RouteProtocol = 198

// routePriorityMelnode is the metric of every kernel route melnode installs.
// What the host set up itself (connected subnets, the default route, the
// uplink's unreachable mesh range) sits at metric 0, so a mesh claim for the
// same prefix goes in beside it and loses, instead of RouteReplace
// overwriting it and a later withdrawal deleting it. Same metric the old
// melinoe-route daemon used.
const routePriorityMelnode = 1

// Programming the kernel's routing table for advertised prefixes (so
// traffic for a prefix goes out the right tun) is done by the reconciler,
// see reconcile.go -- no policy routing, no IPIP tunnels: every prefix's
// traffic already rides inside the right peerid's existing Noise-encrypted
// tun, same as everything else melnode routes.
