// main.go
//
// Loads ct_redirect.bpf.c and dynamically attaches
// ct_redirect_ingress/egress (via tcx) to every non-loopback interface,
// watching netlink for link add/del/rename events so it stays correct as
// interfaces come and go.
//
// --watch-prefix selects which interfaces are RTS ("return to sender") by
// name prefix (comma-separated list of prefixes, e.g. "node-,pub0" --
// matches if a name starts with ANY of them), re-evaluated live: renaming an interface into or out of
// the prefix toggles it on/off without restarting the program. This is
// NOT which interfaces get flows recorded -- recording happens
// unconditionally on every interface, every hook, in the BPF program
// itself (see ct_redirect.bpf.c). What --watch-prefix actually controls:
// if a flow's FIRST-SEEN packet arrived on an RTS-tagged interface, its
// reply is forced back out that same interface, no matter where else it
// would otherwise go. Interfaces not in this set are still recorded (so
// they can be looked up as someone else's reverse tuple), they just
// don't force anything themselves.
//
// --watch-prefix is a stand-in for now; eventually this will be driven
// by melinoe-route telling us the real RTS interface set directly
// instead of matching by name.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -cflags "-O3 -g -Wall -mcpu=v4" ctredirect bpf/ct_redirect.bpf.c

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// l3OnlyLinkKinds are netlink link kinds (Link.Type()) with no L2 header
// at all -- the kernel hands these to tc BPF programs as raw IP packets,
// skb->data pointing straight at the IP header, no ethhdr to skip.
// Everything not in this set (veth, physical/bond NICs, bridges, lo --
// lo carries a real, if fake, 14-byte Ethernet header, confirmed via
// tcpdump showing link-type EN10MB on it) is treated as Ethernet-framed.
var l3OnlyLinkKinds = map[string]bool{
	"ipip":      true,
	"gre":       true, // NOT gretap -- gretap carries an inner Ethernet header
	"sit":       true,
	"vti":       true,
	"wireguard": true,
	"xfrm":      true,
}

func isL3Only(l netlink.Link) bool {
	return l3OnlyLinkKinds[l.Type()]
}

func (m *manager) setL3Only(ifindex int, l3only bool) {
	if l3only {
		if err := m.objs.L3OnlyIfaces.Put(uint32(ifindex), uint8(1)); err != nil {
			log.Printf("l3-only-map add ifindex %d: %v", ifindex, err)
		}
	} else {
		if err := m.objs.L3OnlyIfaces.Delete(uint32(ifindex)); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			log.Printf("l3-only-map remove ifindex %d: %v", ifindex, err)
		}
	}
}

// gcTimeouts holds every timeout value the GC sweep needs. Every field
// is named after, and defaults to, the matching Linux conntrack sysctl
// default (see nf_conntrack-sysctl.rst) -- the goal is to reproduce
// conntrack's own flow-lifetime behavior, not invent new numbers. All
// are runtime-configurable via CLI flags (see main()).
type gcTimeouts struct {
	tcpSynSentTimeout     time.Duration // conntrack nf_conntrack_tcp_timeout_syn_sent
	tcpSynRecvTimeout     time.Duration // conntrack nf_conntrack_tcp_timeout_syn_recv
	tcpEstablishedTimeout time.Duration // conntrack nf_conntrack_tcp_timeout_established
	tcpFinWaitTimeout     time.Duration // conntrack nf_conntrack_tcp_timeout_fin_wait
	tcpCloseWaitTimeout   time.Duration // conntrack nf_conntrack_tcp_timeout_close_wait
	tcpLastAckTimeout     time.Duration // conntrack nf_conntrack_tcp_timeout_last_ack
	tcpTimeWaitTimeout    time.Duration // conntrack nf_conntrack_tcp_timeout_time_wait
	tcpCloseTimeout       time.Duration // conntrack nf_conntrack_tcp_timeout_close
	udpUnrepliedTimeout   time.Duration // conntrack nf_conntrack_udp_timeout
	udpAssuredTimeout     time.Duration // conntrack nf_conntrack_udp_timeout_stream
	icmpTimeout           time.Duration // conntrack nf_conntrack_icmp_timeout
	genericTimeout        time.Duration // conntrack nf_conntrack_generic_timeout
	fragTimeout           time.Duration // conntrack-adjacent: matches Linux's own
	                                    // IP/IPv6 fragment-reassembly timeout
	                                    // (net.ipv4.ipfrag_time / net.ipv6.ip6frag_time)
}

func defaultGCTimeouts() gcTimeouts {
	return gcTimeouts{
		tcpSynSentTimeout:     120 * time.Second,
		tcpSynRecvTimeout:     60 * time.Second,
		tcpEstablishedTimeout: 432000 * time.Second, // 5 days
		tcpFinWaitTimeout:     120 * time.Second,
		tcpCloseWaitTimeout:   60 * time.Second,
		tcpLastAckTimeout:     30 * time.Second,
		tcpTimeWaitTimeout:    120 * time.Second,
		tcpCloseTimeout:       10 * time.Second,
		udpUnrepliedTimeout:   30 * time.Second,
		udpAssuredTimeout:     120 * time.Second,
		icmpTimeout:           30 * time.Second,
		genericTimeout:        600 * time.Second,
		fragTimeout:           30 * time.Second,
	}
}

// shortestInterval returns the sweep interval to use: min(all configured
// timeouts)/4, floored at 1s and ceilinged at 5min. With per-state
// timeouts spanning a much wider range (10s CLOSE vs 5d ESTABLISHED), a
// single sweep interval based on the longest timeout would sweep far too
// infrequently for the short ones to ever be reaped promptly.
func (t gcTimeouts) shortestInterval() time.Duration {
	shortest := t.tcpCloseTimeout
	for _, d := range []time.Duration{
		t.tcpSynSentTimeout, t.tcpSynRecvTimeout, t.tcpEstablishedTimeout,
		t.tcpFinWaitTimeout, t.tcpCloseWaitTimeout, t.tcpLastAckTimeout,
		t.tcpTimeWaitTimeout, t.udpUnrepliedTimeout, t.udpAssuredTimeout,
		t.icmpTimeout, t.genericTimeout, t.fragTimeout,
	} {
		if d < shortest {
			shortest = d
		}
	}
	interval := shortest / 4
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	return interval
}

// nowMonotonicNs reads CLOCK_MONOTONIC, matching bpf_ktime_get_ns()'s
// clock base -- no wall-clock conversion needed or possible, values are
// only ever compared against each other, never displayed as a real time.
func nowMonotonicNs() (uint64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, err
	}
	return uint64(ts.Sec)*1e9 + uint64(ts.Nsec), nil
}

// ageOrSkip returns how long ago baseNs was, or ok=false if baseNs is
// still in the future relative to nowNs (a benign race with the sweep
// snapshot -- the entry was touched between the iterator reading it and
// nowMonotonicNs() being called -- not a stale entry).
func ageOrSkip(nowNs, baseNs uint64) (age time.Duration, ok bool) {
	if nowNs < baseNs {
		return 0, false
	}
	return time.Duration(nowNs-baseNs) * time.Nanosecond, true
}

// sweepSimpleMap handles every flat-timeout bucket type: one timeout, no
// per-entry state to branch on. Generic over the value type too, not
// just the key -- frag4_track/frag6_track no longer share
// ctredirectSimpleFlowVal's shape (they hold cached tuple-completion
// fields, not a redirect ifindex; see ct_redirect.bpf.c), so callers
// pass a lastSeen accessor for whichever value type their map actually
// uses.
func sweepSimpleMap[K any, V any](name string, m *ebpf.Map, nowNs uint64, timeout time.Duration, lastSeen func(V) uint64) {
	var (
		key      K
		val      V
		toDelete []K
	)
	it := m.Iterate()
	for it.Next(&key, &val) {
		age, ok := ageOrSkip(nowNs, lastSeen(val))
		if ok && age > timeout {
			toDelete = append(toDelete, key)
		}
	}
	if err := it.Err(); err != nil {
		log.Printf("%s gc: iterate: %v", name, err)
	}
	for _, k := range toDelete {
		if err := m.Delete(&k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			log.Printf("%s gc: delete: %v", name, err)
		}
	}
	if len(toDelete) > 0 {
		fmt.Printf("%s gc: expired %d flow(s)\n", name, len(toDelete))
	}
}

// TCP state constants -- mirror TCP_S_* in ct_redirect.bpf.c. Kept as
// plain untyped constants (not an enum type) since ctredirectFlowVal.State
// is a bare uint8 from bpf2go's generated code.
const (
	tcpStateSynSent = iota
	tcpStateSynRecv
	tcpStateEstablished
	tcpStateFinWait
	tcpStateCloseWait
	tcpStateLastAck
	tcpStateTimeWait
	tcpStateClose
)

// IP protocol numbers -- mirror the IPPROTO_* #defines in
// ct_redirect.bpf.c. Needed here now that v4_flows/v6_flows are merged
// maps: sweepFlowMap has to read each entry's own key.Proto to know
// which timeout table applies, the same dispatch the BPF side does via
// flow_update()'s proto argument.
const (
	ipprotoICMP   = 1
	ipprotoTCP    = 6
	ipprotoUDP    = 17
	ipprotoICMPv6 = 58
)

// protoHolder is implemented by both key types the merged v4_flows/
// v6_flows maps use -- all sweepFlowMap needs to decide which protocol's
// timeout rules apply to a given entry, since (unlike the old one-map-
// per-protocol layout) that's no longer implied by which map you're
// iterating.
type protoHolder interface {
	protoVal() uint8
}

func (k ctredirectV4FlowKey) protoVal() uint8 { return k.Proto }
func (k ctredirectV6FlowKey) protoVal() uint8 { return k.Proto }

// sweepFlowMap applies conntrack-modeled timeout rules to the merged
// v4_flows/v6_flows maps, branching per-entry on the entry's own
// key.Proto -- TCP gets the full per-state timeout table (see the
// state_since_ns vs last_seen_ns comment below), UDP gets the
// assured/unreplied distinction, everything else (ICMP echo, generic
// bucket) gets its own flat timeout. This replaces the old sweepTCPMap/
// sweepUDPMap/sweepSimpleMap trio now that all four protocols share one
// map per family instead of one map each.
func sweepFlowMap[K protoHolder](name string, m *ebpf.Map, nowNs uint64, t gcTimeouts) {
	var (
		key      K
		val      ctredirectFlowVal
		toDelete []K
	)
	it := m.Iterate()
	for it.Next(&key, &val) {
		var timeout time.Duration
		var base uint64

		switch key.protoVal() {
		case ipprotoTCP:
			// Every state except ESTABLISHED uses state_since_ns --
			// "how long have you been in this state", so a flow that's
			// chatty within a short state (e.g. retransmitting SYNs, or
			// bouncing FIN/ACKs) doesn't dodge that state's own timeout
			// just because each packet refreshes last_seen_ns.
			// ESTABLISHED alone uses last_seen_ns (idle time) -- it's
			// the one state deliberately built to survive long quiet
			// periods (IMAP IDLE and friends), so idle time is exactly
			// what should NOT expire it, matching conntrack's own
			// established-timeout semantics.
			base = val.StateSinceNs
			switch val.State {
			case tcpStateSynSent:
				timeout = t.tcpSynSentTimeout
			case tcpStateSynRecv:
				timeout = t.tcpSynRecvTimeout
			case tcpStateFinWait:
				timeout = t.tcpFinWaitTimeout
			case tcpStateCloseWait:
				timeout = t.tcpCloseWaitTimeout
			case tcpStateLastAck:
				timeout = t.tcpLastAckTimeout
			case tcpStateTimeWait:
				timeout = t.tcpTimeWaitTimeout
			case tcpStateClose:
				timeout = t.tcpCloseTimeout
			default: // tcpStateEstablished
				timeout = t.tcpEstablishedTimeout
				base = val.LastSeenNs
			}
		case ipprotoUDP:
			// Assured (bidirectional traffic seen) gets the longer
			// timeout, unreplied the shorter -- conntrack's own
			// distinction.
			base = val.LastSeenNs
			if val.Assured != 0 {
				timeout = t.udpAssuredTimeout
			} else {
				timeout = t.udpUnrepliedTimeout
			}
		case ipprotoICMP, ipprotoICMPv6:
			base = val.LastSeenNs
			timeout = t.icmpTimeout
		default: // generic bucket -- GRE/ESP/non-echo ICMP/etc
			base = val.LastSeenNs
			timeout = t.genericTimeout
		}

		age, ok := ageOrSkip(nowNs, base)
		if ok && age > timeout {
			toDelete = append(toDelete, key)
		}
	}
	if err := it.Err(); err != nil {
		log.Printf("%s gc: iterate: %v", name, err)
	}
	for _, k := range toDelete {
		if err := m.Delete(&k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			log.Printf("%s gc: delete: %v", name, err)
		}
	}
	if len(toDelete) > 0 {
		fmt.Printf("%s gc: expired %d flow(s)\n", name, len(toDelete))
	}
}

// flowGC periodically sweeps every flow/fragment-cache map, applying
// each map's own timeout logic. This is the actual expiry mechanism --
// the BPF program only ever refreshes timestamps/state and inserts new
// entries, it never deletes anything itself (each map's LRU_HASH type
// provides a capacity-based backstop, not a substitute for real
// timeout-based expiry).
func flowGC(objs *ctredirectObjects, t gcTimeouts, stop <-chan struct{}) {
	ticker := time.NewTicker(t.shortestInterval())
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			nowNs, err := nowMonotonicNs()
			if err != nil {
				log.Printf("flow gc: clock_gettime: %v", err)
				continue
			}
			sweepFlowMap[ctredirectV4FlowKey]("v4_flows", objs.V4Flows, nowNs, t)
			sweepFlowMap[ctredirectV6FlowKey]("v6_flows", objs.V6Flows, nowNs, t)
			fragLastSeen4 := func(v ctredirectFrag4Val) uint64 { return v.LastSeenNs }
			fragLastSeen6 := func(v ctredirectFrag6Val) uint64 { return v.LastSeenNs }
			sweepSimpleMap[ctredirectFrag4Key]("frag4_track", objs.Frag4Track, nowNs, t.fragTimeout, fragLastSeen4)
			sweepSimpleMap[ctredirectFrag6Key]("frag6_track", objs.Frag6Track, nowNs, t.fragTimeout, fragLastSeen6)
		}
	}
}

type attached struct {
	name            string
	ingress, egress link.Link
	watched         bool
}

type manager struct {
	objs     *ctredirectObjects
	prefixes []string

	mu      sync.Mutex
	byIdx   map[int]*attached
	rtsWant map[string]bool // interface names marked RTS via --control-socket
}

func newManager(objs *ctredirectObjects, prefixes []string) *manager {
	return &manager{objs: objs, prefixes: prefixes, byIdx: make(map[int]*attached), rtsWant: make(map[string]bool)}
}

// matches reports whether name should be treated as RTS -- either it
// matches a --watch-prefix, or an external daemon (melinoe-route)
// explicitly added it over --control-socket. The two sources are
// additive, not exclusive, so --watch-prefix keeps working standalone
// for manual/debug use even once melinoe-route drives things directly.
func (m *manager) matches(name string) bool {
	for _, p := range m.prefixes {
		if p != "" && strings.HasPrefix(name, p) {
			return true
		}
	}
	return m.rtsWant[name]
}

// setRTSWant records (or clears) an interface's RTS-via-control-socket
// membership and immediately re-evaluates any already-attached interface
// with that name, mirroring what renameCheck does for prefix changes.
func (m *manager) setRTSWant(name string, want bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if want {
		m.rtsWant[name] = true
	} else {
		delete(m.rtsWant, name)
	}

	for ifindex, a := range m.byIdx {
		if a.name != name {
			continue
		}
		newWatched := m.matches(name)
		if newWatched != a.watched {
			a.watched = newWatched
			m.setWatched(ifindex, newWatched)
			fmt.Printf("%s (ifindex %d) watched=%v (control-socket %s)\n", name, ifindex, newWatched, map[bool]string{true: "add", false: "remove"}[want])
		}
	}
}

// controlMsg is the wire format accepted on --control-socket: one
// newline-delimited JSON object per event, sent by an external daemon
// (melinoe-route) that knows the real RTS interface set directly,
// instead of us guessing it from interface-name prefixes.
type controlMsg struct {
	Op    string `json:"op"`    // "add" or "remove"
	Iface string `json:"iface"` // interface name, e.g. "node-3"
}

// serveControlSocket listens on a unix socket for controlMsg events.
// Multiple concurrent connections are fine -- every message is applied
// independently via setRTSWant, so there's no per-connection state to
// reconcile and no harm in melinoe-route dialing fresh for each event.
func serveControlSocket(path string, m *manager) error {
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		log.Printf("control socket: chmod %s: %v", path, err)
	}
	go func() {
		defer l.Close()
		for {
			conn, err := l.Accept()
			if err != nil {
				log.Printf("control socket: accept: %v", err)
				return
			}
			go handleControlConn(conn, m)
		}
	}()
	return nil
}

func handleControlConn(conn net.Conn, m *manager) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	for {
		var msg controlMsg
		if err := dec.Decode(&msg); err != nil {
			if err != io.EOF {
				log.Printf("control socket: decode: %v", err)
			}
			return
		}
		if msg.Iface == "" {
			continue
		}
		switch msg.Op {
		case "add":
			m.setRTSWant(msg.Iface, true)
		case "remove":
			m.setRTSWant(msg.Iface, false)
		default:
			log.Printf("control socket: unknown op %q", msg.Op)
		}
	}
}

func (m *manager) setWatched(ifindex int, watched bool) {
	if watched {
		if err := m.objs.RtsIfaces.Put(uint32(ifindex), uint8(1)); err != nil {
			log.Printf("watch-map add ifindex %d: %v", ifindex, err)
		}
	} else {
		if err := m.objs.RtsIfaces.Delete(uint32(ifindex)); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			log.Printf("watch-map remove ifindex %d: %v", ifindex, err)
		}
	}
}

func (m *manager) attachIface(l netlink.Link) {
	attrs := l.Attrs()
	ifindex := attrs.Index
	name := attrs.Name

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.byIdx[ifindex]; ok {
		return // already attached
	}

	a := &attached{name: name}

	inL, err := link.AttachTCX(link.TCXOptions{
		Program:   m.objs.CtRedirectIngress,
		Attach:    ebpf.AttachTCXIngress,
		Interface: ifindex,
	})
	if err != nil {
		log.Printf("skipping ingress on %s (ifindex %d): %v", name, ifindex, err)
	} else {
		a.ingress = inL
	}

	egL, err := link.AttachTCX(link.TCXOptions{
		Program:   m.objs.CtRedirectEgress,
		Attach:    ebpf.AttachTCXEgress,
		Interface: ifindex,
	})
	if err != nil {
		log.Printf("skipping egress on %s (ifindex %d): %v", name, ifindex, err)
	} else {
		a.egress = egL
	}

	a.watched = m.matches(name)
	if a.watched {
		m.setWatched(ifindex, true)
	}

	l3only := isL3Only(l)
	m.setL3Only(ifindex, l3only)

	m.byIdx[ifindex] = a
	fmt.Printf("attached to %s (ifindex %d) watched=%v l3only=%v\n", name, ifindex, a.watched, l3only)
}

// ifindexHolder is implemented by the merged flow-value type
// (ctredirectFlowVal, shared by both v4_flows and v6_flows) -- all
// sweepStaleFlowsInMap needs is a way to read Ifindex back out.
//
// Deliberately NOT implemented by ctredirectFrag4Val/ctredirectFrag6Val:
// those no longer cache a redirect decision at all, just the L4 fields
// (ports/echo-id) needed to complete a later fragment's lookup key (see
// ct_redirect.bpf.c) -- there is no ifindex in them to go stale, so
// frag4_track/frag6_track don't need sweeping on interface detach
// anymore. A stale port-pair entry just gets looked up against
// whatever's in the real flow table at that moment, same as any other
// fragment continuation, so it can't misdirect traffic the way a cached
// ifindex could.
type ifindexHolder interface {
	ifindexVal() uint32
}

func (v ctredirectFlowVal) ifindexVal() uint32 { return v.Ifindex }

// sweepStaleFlowsInMap removes any entries in m recorded against
// ifindex, regardless of which map m is or what state/timeout fields its
// value type carries beyond Ifindex -- V's ifindexVal() is all this
// needs.
func sweepStaleFlowsInMap[K any, V ifindexHolder](name string, m *ebpf.Map, ifindex uint32) {
	var (
		key      K
		val      V
		toDelete []K
	)
	it := m.Iterate()
	for it.Next(&key, &val) {
		if val.ifindexVal() == ifindex {
			toDelete = append(toDelete, key)
		}
	}
	if err := it.Err(); err != nil {
		log.Printf("%s sweep on detach: iterate: %v", name, err)
	}
	for _, k := range toDelete {
		if err := m.Delete(&k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			log.Printf("%s sweep on detach: delete: %v", name, err)
		}
	}
	if len(toDelete) > 0 {
		fmt.Printf("%s sweep: dropped %d stale flow(s) recorded against ifindex %d\n", name, len(toDelete), ifindex)
	}
}

// sweepStaleFlows removes any entry, in either flow map, recorded
// against ifindex. Called on interface detach so that if this ifindex
// number later gets reused by an unrelated interface, no leftover entry
// can cause traffic to be redirected somewhere it was never meant to
// go. This is a correctness fix, not just cleanup -- neither map has any
// other mechanism tied to interface lifecycle, and relying on GC's idle/
// state timeouts alone would leave a window where a reused ifindex is
// live but a stale entry (potentially one sitting at the 5-day
// ESTABLISHED timeout) hasn't expired yet.
func (m *manager) sweepStaleFlows(ifindex int) {
	idx := uint32(ifindex)
	sweepStaleFlowsInMap[ctredirectV4FlowKey, ctredirectFlowVal]("v4_flows", m.objs.V4Flows, idx)
	sweepStaleFlowsInMap[ctredirectV6FlowKey, ctredirectFlowVal]("v6_flows", m.objs.V6Flows, idx)
	// frag4_track/frag6_track intentionally not swept here -- see the
	// ifindexHolder comment above.
}

func (m *manager) detachIface(ifindex int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.byIdx[ifindex]
	if !ok {
		return
	}
	if a.ingress != nil {
		a.ingress.Close()
	}
	if a.egress != nil {
		a.egress.Close()
	}
	if a.watched {
		m.setWatched(ifindex, false)
	}
	m.setL3Only(ifindex, false)
	delete(m.byIdx, ifindex)
	m.sweepStaleFlows(ifindex)
	fmt.Printf("detached from %s (ifindex %d)\n", a.name, ifindex)
}

// renameCheck re-evaluates the watch state for an already-attached
// interface whose name may have changed.
func (m *manager) renameCheck(ifindex int, newName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.byIdx[ifindex]
	if !ok {
		return
	}
	a.name = newName
	want := m.matches(newName)
	if want == a.watched {
		return
	}
	a.watched = want
	m.setWatched(ifindex, want)
	fmt.Printf("%s (ifindex %d) watched=%v (renamed)\n", newName, ifindex, want)
}

// mapSizeBudget describes how the total flow-tracking budget (an
// analog of conntrack's single nf_conntrack_max) is split across this
// program's four maps: v4_flows, v6_flows, frag4_track, frag6_track.
// v4_flows/v6_flows now hold every protocol (TCP/UDP/ICMP echo/generic)
// together within their family -- see the comment on v4_flow_key in
// ct_redirect.bpf.c for why -- so unlike the original ten-map layout,
// a UDP or ICMP flood CAN now evict a TCP entry within the same family.
// Deliberate trade for this fleet specifically: see the proto breakdown
// below, TCP dominates real entry counts heavily enough that the
// isolation being given up mostly only protected the 87% case from the
// combined <13% case, not the reverse.
//
// The 84/12/1/1/2 proto-level split (tcp/udp/icmp/generic/frag) is
// pulled from an actual `conntrack -L` proto breakdown on a real box in
// this fleet (857 tcp / 119 udp / 7 unknown / 3 icmp out of 986,
// observed floating 0.25x-4x that baseline with load) -- not a guess.
// icmp+generic get a full percentage point each anyway despite rounding
// near-zero in that sample, since low-traffic doesn't mean zero-traffic.
// frag4/6_track gets more than its measured share (there's no
// "fragment" row in conntrack's output to measure against -- it isn't a
// protocol) because it's the table most likely to matter exactly when
// it's under the most pressure: a burst of concurrent fragmented flows.
// Those four proto-level numbers are summed directly into v4_flows'/
// v6_flows' single weight each, split v4/v6 at roughly the same 56/44
// ratio the original ten-map placeholder used -- no real per-family
// breakdown available yet, revisit if `conntrack -L` output (or an
// equivalent) is ever captured split by family instead of just by proto.
var mapSizeWeights = map[string]float64{
	"v4_flows":    0.547, // tcp4 .470 + udp4 .067 + icmp4 .005 + generic4 .005
	"v6_flows":    0.433, // tcp6 .370 + udp6 .053 + icmp6 .005 + generic6 .005
	"frag4_track": 0.010,
	"frag6_track": 0.010,
}

// minMapEntries is a floor under every table regardless of how small
// the overall budget is -- protects the small/weird tables (frag_track
// especially) from rounding down to something degenerate on a tiny
// override.
const minMapEntries = 256

// defaultConntrackMax is the fallback total budget when
// /proc/sys/net/netfilter/nf_conntrack_max can't be read (nf_conntrack
// module never loaded, running in a container without /proc mounted,
// etc). Matches the modern-kernel default for a >4GB box, not the
// smallest end of the fleet -- better to over-provision on a box we
// couldn't actually measure than to quietly under-provision it.
const defaultConntrackMax = 262144

// readConntrackMax reads this host's own net.netfilter.nf_conntrack_max,
// the same number you'd get from `sysctl net.netfilter.nf_conntrack_max`
// -- used as the default total budget so each box sizes itself off
// whatever conntrack itself would have used there, no per-node flag
// needed. Returns ok=false if the file doesn't exist (nf_conntrack
// module never loaded) or won't parse, so the caller can fall back to a
// fixed default.
func readConntrackMax() (uint32, bool) {
	raw, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_max")
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 32)
	if err != nil || v == 0 {
		return 0, false
	}
	return uint32(v), true
}

// resizeMapSpecs applies mapSizeWeights against totalBudget to every map
// in spec, in place, before the collection is loaded -- MaxEntries can
// only be set pre-load, cilium/ebpf has no post-load resize. A map name
// not present in mapSizeWeights (shouldn't happen -- this covers all
// four SEC(".maps") flow/fragment-tracking definitions in
// ct_redirect.bpf.c) is left at whatever max_entries the C source itself
// declared. (l3_only_ifaces/rts_ifaces are config maps, not sized off
// this budget at all -- untouched here.)
func resizeMapSpecs(spec *ebpf.CollectionSpec, totalBudget uint32) {
	for name, weight := range mapSizeWeights {
		m, ok := spec.Maps[name]
		if !ok {
			continue
		}
		entries := uint32(float64(totalBudget) * weight)
		if entries < minMapEntries {
			entries = minMapEntries
		}
		m.MaxEntries = entries
	}
}

func main() {
	prefixFlag := flag.String("watch-prefix", "", "comma-separated interface name prefixes to mark as RTS (return-to-sender), e.g. node-,pub0 (at least one of --watch-prefix/--control-socket is required)")
	controlSocketFlag := flag.String("control-socket", "", "unix socket path to accept RTS add/remove events on from an external daemon (e.g. melinoe-route), additive to --watch-prefix (at least one of --watch-prefix/--control-socket is required)")

	defaults := defaultGCTimeouts()
	tcpSynSentFlag := flag.Duration("tcp-syn-sent-timeout", defaults.tcpSynSentTimeout,
		"delete a TCP flow entry still waiting on a SYN-ACK after this long (conntrack: tcp_timeout_syn_sent)")
	tcpSynRecvFlag := flag.Duration("tcp-syn-recv-timeout", defaults.tcpSynRecvTimeout,
		"delete a TCP flow entry still waiting on the handshake's final ACK after this long (conntrack: tcp_timeout_syn_recv)")
	tcpEstablishedFlag := flag.Duration("tcp-established-timeout", defaults.tcpEstablishedTimeout,
		"delete an established TCP flow entry if it hasn't seen a packet in this long -- this is the one that needs to comfortably outlast things like IMAP IDLE (conntrack: tcp_timeout_established)")
	tcpFinWaitFlag := flag.Duration("tcp-fin-wait-timeout", defaults.tcpFinWaitTimeout,
		"delete a TCP flow entry this long after the first FIN was observed (conntrack: tcp_timeout_fin_wait)")
	tcpCloseWaitFlag := flag.Duration("tcp-close-wait-timeout", defaults.tcpCloseWaitTimeout,
		"delete a TCP flow entry this long after the not-yet-FIN'd side acknowledges the close (conntrack: tcp_timeout_close_wait)")
	tcpLastAckFlag := flag.Duration("tcp-last-ack-timeout", defaults.tcpLastAckTimeout,
		"delete a TCP flow entry this long after both sides have sent a FIN (conntrack: tcp_timeout_last_ack)")
	tcpTimeWaitFlag := flag.Duration("tcp-time-wait-timeout", defaults.tcpTimeWaitTimeout,
		"delete a TCP flow entry this long after entering TIME_WAIT (conntrack: tcp_timeout_time_wait)")
	tcpCloseFlag := flag.Duration("tcp-close-timeout", defaults.tcpCloseTimeout,
		"delete a TCP flow entry this long after an RST was observed (conntrack: tcp_timeout_close)")
	udpUnrepliedFlag := flag.Duration("udp-unreplied-timeout", defaults.udpUnrepliedTimeout,
		"delete a UDP flow entry that has only ever seen one-directional traffic after this long (conntrack: udp_timeout)")
	udpAssuredFlag := flag.Duration("udp-assured-timeout", defaults.udpAssuredTimeout,
		"delete a UDP flow entry that has seen bidirectional traffic if it hasn't seen a packet in this long (conntrack: udp_timeout_stream)")
	icmpTimeoutFlag := flag.Duration("icmp-timeout", defaults.icmpTimeout,
		"delete an ICMP echo flow entry if it hasn't seen a packet in this long (conntrack: icmp_timeout)")
	genericTimeoutFlag := flag.Duration("generic-timeout", defaults.genericTimeout,
		"delete a generic-bucket (GRE/ESP/raw ipip/non-echo ICMP/etc) flow entry if it hasn't seen a packet in this long (conntrack: generic_timeout)")
	fragTimeoutFlag := flag.Duration("frag-timeout", defaults.fragTimeout,
		"delete a fragment-tracking cache entry this long after its first fragment was seen (matches Linux's own IP/IPv6 fragment-reassembly timeout)")
	conntrackMaxFlag := flag.Uint("conntrack-max", 0,
		"total flow-tracking budget, split across v4_flows/v6_flows/frag4_track/frag6_track by mapSizeWeights (like conntrack's nf_conntrack_max, but v4 and v6 are still separate pools rather than one shared table). 0 (default) reads this host's own /proc/sys/net/netfilter/nf_conntrack_max and uses that; falls back to 262144 if unreadable (module not loaded, container without /proc, etc)")
	flag.Parse()

	if *prefixFlag == "" && *controlSocketFlag == "" {
		log.Fatal("at least one of --watch-prefix or --control-socket is required")
	}
	var prefixes []string
	if *prefixFlag != "" {
		prefixes = strings.Split(*prefixFlag, ",")
	}

	timeouts := gcTimeouts{
		tcpSynSentTimeout:     *tcpSynSentFlag,
		tcpSynRecvTimeout:     *tcpSynRecvFlag,
		tcpEstablishedTimeout: *tcpEstablishedFlag,
		tcpFinWaitTimeout:     *tcpFinWaitFlag,
		tcpCloseWaitTimeout:   *tcpCloseWaitFlag,
		tcpLastAckTimeout:     *tcpLastAckFlag,
		tcpTimeWaitTimeout:    *tcpTimeWaitFlag,
		tcpCloseTimeout:       *tcpCloseFlag,
		udpUnrepliedTimeout:   *udpUnrepliedFlag,
		udpAssuredTimeout:     *udpAssuredFlag,
		icmpTimeout:           *icmpTimeoutFlag,
		genericTimeout:        *genericTimeoutFlag,
		fragTimeout:           *fragTimeoutFlag,
	}

	// Defensive for older kernels (pre-~5.11) that enforce RLIMIT_MEMLOCK
	// for BPF map creation. No-op/harmless on newer kernels where memcg-
	// based BPF accounting makes this unnecessary -- cilium/ebpf ships
	// this exact helper for this exact purpose.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Printf("removing memlock rlimit (continuing anyway): %v", err)
	}

	conntrackMax := uint32(*conntrackMaxFlag)
	if conntrackMax == 0 {
		if v, ok := readConntrackMax(); ok {
			conntrackMax = v
			log.Printf("sizing flow tables off this host's nf_conntrack_max: %d", conntrackMax)
		} else {
			conntrackMax = defaultConntrackMax
			log.Printf("nf_conntrack_max unreadable, falling back to default flow-table budget: %d", conntrackMax)
		}
	}

	spec, err := loadCtredirect()
	if err != nil {
		log.Fatalf("loading eBPF spec: %v", err)
	}
	resizeMapSpecs(spec, conntrackMax)

	var objs ctredirectObjects
	if err := spec.LoadAndAssign(&objs, nil); err != nil {
		log.Fatalf("loading eBPF objects: %v", err)
	}
	defer objs.Close()

	gcStop := make(chan struct{})
	go flowGC(&objs, timeouts, gcStop)
	defer close(gcStop)

	m := newManager(&objs, prefixes)

	if *controlSocketFlag != "" {
		if err := serveControlSocket(*controlSocketFlag, m); err != nil {
			log.Fatalf("control socket: %v", err)
		}
		log.Printf("accepting RTS add/remove events on %s", *controlSocketFlag)
	}

	// Initial snapshot.
	links, err := netlink.LinkList()
	if err != nil {
		log.Fatalf("listing links: %v", err)
	}
	for _, l := range links {
		m.attachIface(l)
	}
	defer func() {
		m.mu.Lock()
		for idx := range m.byIdx {
			// detachIface takes the lock itself; collect then release.
			_ = idx
		}
		m.mu.Unlock()
		for idx := range m.byIdx {
			m.detachIface(idx)
		}
	}()

	// Live updates.
	updates := make(chan netlink.LinkUpdate)
	done := make(chan struct{})
	defer close(done)
	if err := netlink.LinkSubscribe(updates, done); err != nil {
		log.Fatalf("subscribing to link updates: %v", err)
	}

	sig := make(chan os.Signal, 1)
	// SIGTERM matters for running under systemd, which sends SIGTERM on
	// stop by default, not SIGINT -- without this, a normal `systemctl
	// stop` would not trigger the deferred detach/cleanup above at all.
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	fmt.Printf("watching for interfaces with RTS prefixes %v (control-socket=%q) -- Ctrl-C to exit\n", prefixes, *controlSocketFlag)

	for {
		select {
		case <-sig:
			return
		case u, ok := <-updates:
			if !ok {
				return
			}
			name := u.Attrs().Name
			ifindex := u.Attrs().Index
			switch {
			case u.Header.Type == 16: // RTM_NEWLINK
				m.mu.Lock()
				_, exists := m.byIdx[ifindex]
				m.mu.Unlock()
				if exists {
					m.renameCheck(ifindex, name)
				} else {
					m.attachIface(u.Link)
				}
			case u.Header.Type == 17: // RTM_DELLINK
				m.detachIface(ifindex)
			}
		}
	}
}
