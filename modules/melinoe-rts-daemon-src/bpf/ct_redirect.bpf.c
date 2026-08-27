// ct_redirect.bpf.c
//
// Per-flow conntrack-style redirect: records which interface a flow's
// forward direction arrived on, and force-redirects reply-direction
// traffic back out that interface via bpf_redirect_neigh. Attached to
// both tc ingress and tc egress on every interface -- see the comment
// above ct_redirect_ingress/egress for why both hooks are required and
// exactly what is_ingress gates.
//
// One merged flow map per address family (v4_flows, v6_flows) holding
// TCP/UDP/ICMP-echo/generic-bucket flows together -- see the comment on
// v4_flow_key for why they're merged within a family but not across
// families -- plus frag4_track/frag6_track holding the L4 fields
// (ports/echo-id) non-initial IP fragments are missing, so they can
// rejoin the real flow table as themselves instead of via a separately
// cached decision (see frag4_cache_tuple/handle_v4_frag_cont and their
// v6 counterparts).
//
// TCP state machine and every GC timeout (see gcTimeouts in main.go)
// are intentionally modeled on Linux conntrack's own TCP/UDP/ICMP/
// generic/fragment defaults -- the goal is to reproduce conntrack's
// flow-lifetime behavior as closely as possible, not invent new
// semantics that could behave differently from what's already been
// shaken out in conntrack over decades.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>
/* NOT #include <bpf/bpf_kfuncs.h> -- see the "kernel conntrack kfunc
 * declarations" comment further down for the hand-declared kfunc
 * surface and why it's hand-declared rather than pulled from either
 * of those. */

#define TC_ACT_OK    0
#define ETH_P_IP     0x0800
#define ETH_P_IPV6   0x86DD
#define IPPROTO_ICMP 1
#define IPPROTO_TCP  6
#define IPPROTO_UDP  17
#define IPPROTO_ICMPV6 58

#define ICMP_ECHO       8   /* v4 echo request */
#define ICMP_ECHOREPLY  0   /* v4 echo reply   */
#define ICMPV6_ECHO_REQUEST 128
#define ICMPV6_ECHO_REPLY   129

/* IPv6 extension header types we know how to skip over or detect.
 * AH/ESP/MH/anything else stops the walk and is reported as the
 * terminal protocol -- not unwrapped, same as today's generic-bucket
 * treatment, just reached correctly instead of via a misparsed nexthdr. */
#define IPPROTO_HOPOPTS  0
#define IPPROTO_ROUTING  43
#define IPPROTO_FRAGMENT 44
#define IPPROTO_DSTOPTS  60
#define IPV6_MAX_EXT_HDRS 8 /* bounds the walk for the verifier */

/* tcp_state values -- stored in struct flow_val.state. Modeled
 * directly on conntrack's own TCP FSM (see nf_conntrack_proto_tcp.c);
 * each state's GC timeout in main.go's gcTimeouts is named after and
 * defaulted to conntrack's own sysctl default for that state. */
#define TCP_S_SYN_SENT    0 /* conntrack SYN_SENT */
#define TCP_S_SYN_RECV    1 /* conntrack SYN_RECV */
#define TCP_S_ESTABLISHED 2 /* conntrack ESTABLISHED */
#define TCP_S_FIN_WAIT    3 /* conntrack FIN_WAIT */
#define TCP_S_CLOSE_WAIT  4 /* conntrack CLOSE_WAIT */
#define TCP_S_LAST_ACK    5 /* conntrack LAST_ACK */
#define TCP_S_TIME_WAIT   6 /* conntrack TIME_WAIT */
#define TCP_S_CLOSE       7 /* conntrack CLOSE -- terminal */

/* fin_flags bits: which direction(s) have sent a FIN so far. Needed to
 * tell "the same side retransmitting its FIN" apart from "the other
 * side has now also FIN'd" without a second lookup. */
#define TCP_FIN_FWD 0x1
#define TCP_FIN_REV 0x2

/* seq_flags bits: whether fwd_seq_hi/rev_seq_hi has ever been set for
 * that direction -- see the flow_val comment. */
#define TCP_SEQ_FWD_SET 0x1
#define TCP_SEQ_REV_SET 0x2

/* Tolerance window (in sequence-number space) for the RST spoofing
 * check in tcp_advance_state -- see that function's comment for why
 * this exists at all. ~16MB is generous relative to any realistic
 * bandwidth-delay product this box will see, while still narrowing an
 * off-path attacker's blind guess down to a small slice of the full 4GB
 * sequence space instead of accepting anything -- not full RFC 5961
 * window validation (we don't track the advertised receive window),
 * just enough to reject the common "seq=0 or random garbage" blind-
 * spoof case cheaply. */
#define TCP_SEQ_SLACK (1 << 24)

/* --- key/value struct definitions --- */

/* v4_flows/v6_flows hold every non-fragment-completion flow -- TCP, UDP,
 * ICMP echo, and the generic bucket (GRE/ESP/non-echo ICMP/etc) -- in
 * one LRU map per address family, instead of one map per protocol. This
 * deliberately gives up protocol isolation (a UDP/ICMP flood CAN now
 * evict TCP entries, where it couldn't when each protocol had its own
 * map) in exchange for a single shared budget that self-balances across
 * whatever the real traffic mix turns out to be -- worth it here
 * specifically because TCP dominates real-world entry counts by a wide
 * margin (see mapSizeWeights in main.go), so the isolation being given
 * up was protecting the 87% case from the <1% case, not the reverse.
 *
 * v4 and v6 stay in separate maps -- same reasoning as before this
 * merge: a v6 tuple's addresses alone are 4x a v4 tuple's, and merging
 * the families would force every v4 entry's key to carry that overhead
 * it doesn't need. Merging *within* a family costs nothing shape-wise,
 * since sport/dport/proto already have to exist in the key for TCP/UDP
 * either way -- ICMP echo just reuses sport as its id (dport unused),
 * and the generic bucket reuses neither (sport=dport=0, proto alone is
 * the rest of the key, exactly as generic4_key/generic6_key did
 * before). proto is always part of the key, so there's no cross-
 * protocol collision risk even in the sport=0/dport=0 corner case. */
struct v4_flow_key {
    __u32 saddr;
    __u32 daddr;
    __u16 sport;   /* TCP/UDP port, or ICMP echo id; 0 for generic bucket */
    __u16 dport;   /* TCP/UDP port; 0 for ICMP echo and generic bucket */
    __u8  proto;
    __u8  pad[3];
};

struct v6_flow_key {
    struct in6_addr saddr;
    struct in6_addr daddr;
    __u16 sport;
    __u16 dport;
    __u8  proto;
    __u8  pad[3];
};

/* Union of every field any of TCP/UDP/ICMP/generic actually uses.
 * state/fin_flags/state_since_ns/seq_flags/fwd_seq_hi/rev_seq_hi are
 * TCP-only; assured is UDP-only; ICMP echo and generic use only
 * ifindex/last_seen_ns. Shared, byte-identical struct for both v4_flows
 * and v6_flows -- the address family lives in the key, not the value,
 * so one Go type covers both maps. */
struct flow_val {
    __u32 ifindex;         /* live: overwritten on every forward packet,
                             * gated on RTS membership -- see flow_update. */
    __u8  state;            /* TCP_S_* -- TCP only */
    __u8  fin_flags;       /* TCP_FIN_* bitmask -- TCP only */
    __u8  assured;          /* UDP only: 0 = unreplied, 1 = bidirectional
                              * traffic observed -- conntrack's own
                              * distinction, mirrored here. */
    __u8  seq_flags;        /* TCP_SEQ_*_SET bitmask -- TCP only: whether
                              * fwd_seq_hi/rev_seq_hi has a real baseline
                              * yet (can't just check "!= 0", a genuine
                              * ISN can legitimately be 0). See
                              * tcp_advance_state. */
    __u64 last_seen_ns;
    __u64 state_since_ns;   /* TCP only; written on every state
                              * transition -- GC uses this (not
                              * last_seen_ns) for every TCP state except
                              * ESTABLISHED. Unused (left at 0) for
                              * everything else. */
    __u32 fwd_seq_hi;       /* TCP only: highest sequence number
                              * observed in the flow's own forward
                              * direction -- used only to sanity-check a
                              * RST's sequence number before honoring it,
                              * see tcp_advance_state/TCP_SEQ_SLACK. */
    __u32 rev_seq_hi;       /* same, for the reply direction. */
};

/* ICMP echo keys on id instead of sport/dport -- request and reply
 * share the same 16-bit id field by construction, which is exactly why
 * v4_flow_key/v6_flow_key can reuse the sport slot for it and keep the
 * existing symmetric forward/reverse matching logic unmodified. */

/* Fragment tracking: keyed on the fields every fragment of one datagram
 * shares. v4's id is 16 bits (IP header); v6's is 32 bits (Fragment
 * extension header) -- genuinely different widths, not a stylistic
 * choice. */
struct frag4_key {
    __u32 saddr;
    __u32 daddr;
    __u16 id;
    __u8  proto;
    __u8  pad;
};

struct frag6_key {
    struct in6_addr saddr;
    struct in6_addr daddr;
    __be32 id;
    __u8   proto;
    __u8   pad[3];
};

/* Fragment tracking value: NOT a cached redirect decision. It holds only
 * what a non-first fragment is physically missing -- the L4 header
 * fields needed to complete the same lookup key a normal packet would
 * use -- so that later fragments look themselves up in the real flow
 * table (v4_flows/v6_flows) and go through the exact same flow_update()/
 * redirect_target() path as any other packet. Nothing here is ever used
 * to decide the redirect directly; the redirect is always recomputed
 * live from the flow entry these fields let us find. Populated only for
 * protocols whose flow key needs more than address+proto (TCP/UDP
 * ports, ICMP echo id) -- see the comment on
 * frag4_cache_tuple/frag6_cache_tuple. Kept as its own pair of maps
 * rather than folded into v4_flows/v6_flows -- it's tuple-completion
 * data with a totally different key shape (fragment id, not ports), not
 * flow state, and mixing the two would need a discriminated value type
 * for no real benefit. */
struct frag4_val {
    __u8   proto;   /* IPPROTO_TCP/UDP/ICMP -- which flow table to use */
    __u8   pad[3];
    __be16 sport;   /* TCP/UDP source port; ICMP echo id (dport unused) */
    __be16 dport;   /* TCP/UDP dest port; unused for ICMP */
    __u64  last_seen_ns;
};

struct frag6_val {
    __u8   proto;
    __u8   pad[3];
    __be16 sport;
    __be16 dport;
    __u64  last_seen_ns;
};

/* --- interface config maps (unchanged) --- */

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u32);
    __type(value, __u8);
} l3_only_ifaces SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u32);
    __type(value, __u8);
} rts_ifaces SEC(".maps");

/* --- flow maps ---
 *
 * One merged map per address family (v4_flows/v6_flows) holding every
 * TCP/UDP/ICMP-echo/generic-bucket flow together -- see the comment on
 * v4_flow_key above for why. max_entries here is a placeholder; main.go
 * resizes every one of these (via resizeMapSpecs/mapSizeWeights) before
 * load, off this host's own conntrack budget. */

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, struct v4_flow_key);
    __type(value, struct flow_val);
} v4_flows SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 131072);
    __type(key, struct v6_flow_key);
    __type(value, struct flow_val);
} v6_flows SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 4096);
    __type(key, struct frag4_key);
    __type(value, struct frag4_val);
} frag4_track SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 4096);
    __type(key, struct frag6_key);
    __type(value, struct frag6_val);
} frag6_track SEC(".maps");

/* --- shared helpers --- */

static __always_inline __u8 rts_tagged(__u32 ifindex)
{
    return bpf_map_lookup_elem(&rts_ifaces, &ifindex) != NULL;
}

/* Computes the redirect target ifindex for a packet whose reverse-tuple
 * entry recorded rev_ifindex (0 = no redirect: either the reverse entry
 * doesn't exist, or it points at the interface this packet is already
 * on/leaving via, or that interface isn't RTS-tagged). */
static __always_inline __u32 redirect_target(__u32 rev_ifindex, __u32 cur_ifindex)
{
    if (rev_ifindex && rev_ifindex != cur_ifindex && rts_tagged(rev_ifindex))
        return rev_ifindex;
    return 0;
}

static __always_inline int do_redirect_or_ok(__u32 target_ifindex)
{
    if (target_ifindex)
        return bpf_redirect_neigh(target_ifindex, NULL, 0, 0);
    return TC_ACT_OK;
}

/* --- kernel conntrack kfunc declarations ---
 *
 * Hand-declared, not relying on vmlinux.h. Tried merging
 * nf_conntrack's own module BTF into vmlinux.h at build time, twice,
 * correctly located both times (first via config.system.modulesTree,
 * then via the cleaner config.boot.kernelPackages.kernel.modules) --
 * `bpftool btf dump ... format c` never reconstructed usable C
 * declarations from it either time; struct bpf_ct_opts came back with
 * no declaration at all (not even a forward decl) and the three
 * kfuncs were entirely undeclared, apparently because bpftool's C
 * dumper drops types/funcs only reachable through a kfunc's
 * FUNC_PROTO parameter list, regardless of which specific .ko is fed
 * in. See melinoe-rts.nix's vmlinuxH derivation for the fuller
 * writeup. Hand-declaring here is simply the only approach that
 * actually compiles.
 *
 * struct nf_conn stays a bare opaque forward-decl -- we only ever
 * pass the pointer through to bpf_ct_change_status/bpf_ct_release,
 * never dereference a field, so we don't need its real layout. The
 * kfuncs themselves are resolved by cilium/ebpf against the *running*
 * kernel's BTF at load time (module BTF included -- that's the whole
 * point of __ksym), not at compile time, so this works whether
 * nf_conntrack is builtin or a module on the box this actually runs
 * on -- PROVIDED the daemon's process actually has CAP_SYS_ADMIN
 * (cilium/ebpf's module-BTF search needs it specifically, not just
 * CAP_BPF -- see melinoe-rts.nix's systemd unit comment on that
 * capability, and https://github.com/cilium/ebpf/issues/1929).
 * struct bpf_ct_opts's layout (and NF_BPF_CT_OPTS_SZ=12) has been
 * stable since it was introduced in v6.0 (see
 * net/netfilter/nf_conntrack_bpf.c upstream) -- if a future kernel
 * changes it, this needs updating right alongside it, same as any
 * other kernel ABI dependency in this file.
 *
 * (No CO-RE relocation concern either way: every fleet host runs the
 * exact kernel derivation this was compiled against, per flake.lock,
 * so there's no cross-version struct-layout drift to guard against
 * the way upstream selftests' bpf_ct_opts___local exists for.) */
struct bpf_ct_opts {
    __s32 netns_id;
    __s32 error;
    __u8  l4proto;
    __u8  dir;
    __u8  reserved[2];
};

struct nf_conn; /* opaque -- see comment above */

extern struct nf_conn *bpf_skb_ct_lookup(struct __sk_buff *skb_ctx, struct bpf_sock_tuple *bpf_tuple,
                                          __u32 tuple__sz, struct bpf_ct_opts *opts, __u32 opts__sz) __ksym;
extern int bpf_ct_change_status(struct nf_conn *nfct, __u32 status) __ksym;
extern void bpf_ct_release(struct nf_conn *nfct) __ksym;

/* status bits from uapi/linux/netfilter/nf_conntrack_common.h. Kept
 * hand-declared regardless of the above -- these are preprocessor
 * #defines in upstream, and BTF dumps types, not preprocessor text,
 * so they don't survive a `bpftool btf dump ... format c` round-trip
 * regardless of vmlinux.h's contents -- hand-declaring is the only
 * option here, not a fallback for the kfunc question above. Guarded
 * in case a future toolchain/vmlinux.h vendors them some other way. */
#ifndef IPS_SEEN_REPLY
#define IPS_SEEN_REPLY_BIT 1
#define IPS_SEEN_REPLY     (1 << IPS_SEEN_REPLY_BIT)
#endif
#ifndef IPS_ASSURED
#define IPS_ASSURED_BIT 2
#define IPS_ASSURED     (1 << IPS_ASSURED_BIT)
#endif

/* enum ip_conntrack_dir values, from
 * uapi/linux/netfilter/nf_conntrack_tuple_common.h. Same hand-declare
 * reasoning as the IPS_* bits above. IP_CT_DIR_ORIGINAL is the only
 * one this file actually uses (see ct_sync_v4/v6's tuple-orientation
 * comment below for why) -- IP_CT_DIR_REPLY kept declared alongside
 * it for readability/documentation, not because anything here passes
 * it. */
#ifndef IP_CT_DIR_ORIGINAL
#define IP_CT_DIR_ORIGINAL 0
#endif
#ifndef IP_CT_DIR_REPLY
#define IP_CT_DIR_REPLY 1
#endif

/* --- kernel conntrack status sync ---
 *
 * v4_flows/v6_flows above are our own bookkeeping and are what actually
 * drives the redirect decision -- this section doesn't touch that.
 * It's a separate, best-effort nudge to the *kernel's* real
 * nf_conntrack entry for the same 5-tuple, for anything downstream
 * that reads real conntrack (conntrack -L, iptables/nftables ctstate
 * matching, etc.) so it sees the same IPS_SEEN_REPLY/IPS_ASSURED it
 * would have without this program in the path. Failure to find/update
 * the kernel entry is silently ignored -- it never affects the
 * redirect decision.
 *
 * TCP/UDP only -- bpf_skb_ct_lookup() itself rejects any other
 * l4proto with -EPROTO (see bpf_nf_ct_tuple_parse() in
 * net/netfilter/nf_conntrack_bpf.c: "l4proto isn't one of IPPROTO_TCP
 * or IPPROTO_UDP"), so there's no ICMP/GRE/ESP/generic-bucket variant
 * of this to add -- the kfunc can't look those up at all, regardless
 * of what tuple we hand it.
 *
 * The tuple handed to bpf_skb_ct_lookup() is the flow's ORIGINAL
 * (forward) direction tuple -- i.e. our own flow table's rkey, the
 * *reverse* of the packet we're currently holding -- paired with
 * dir=IP_CT_DIR_ORIGINAL, not the packet's own raw src/dst. This
 * looks backwards at first (we're syncing off a reply packet; why
 * pass the original direction?) but bpf_nf_ct_tuple_parse() actually
 * performs a real src/dst swap whenever dir=IP_CT_DIR_REPLY is used,
 * not a pass-through -- so handing it the reply packet's own raw
 * fields with dir=IP_CT_DIR_REPLY produces a tuple matching neither
 * the stored original tuple (right values, wrong dir) nor the stored
 * reply tuple (right dir, swapped values): a guaranteed lookup miss,
 * silently returning ct=NULL and syncing nothing, which is exactly
 * what an earlier version of this file did. dir=IP_CT_DIR_ORIGINAL
 * with the reversed/original-direction tuple lands exactly on the
 * stored original tuple instead, which resolves to the same nf_conn
 * either way -- and matches the only calling convention the kernel's
 * own selftest (net/netfilter/nf_conntrack_bpf.c's associated
 * selftest) actually exercises; it never sets dir at all.
 *
 * status bit semantics, mirrored from our own flow_val fields:
 *   - IPS_SEEN_REPLY: set the moment we redirect a packet that matched
 *     an existing flow's *reverse* tuple -- i.e. a genuine reply has
 *     now been seen, same event as our own is_fwd_dir==0 case in
 *     flow_update().
 *   - IPS_ASSURED: kernel conntrack's own bar for this is "this flow
 *     has proven itself, don't be first against the wall under table
 *     pressure" -- TCP_S_ESTABLISHED is our equivalent for TCP (real
 *     data exchanged both ways, not just a handshake in flight). For
 *     UDP we don't model conntrack's stricter "assured" semantics
 *     (conntrack's UDP tracker actually only asserts it once a *third*
 *     packet arrives continuing the flow after a reply was seen, to
 *     avoid marking single request/response probes assured); we reuse
 *     our own e->assured's simpler "first reply seen" bar instead,
 *     same as the comment on flow_val.assured already documents. If
 *     that mismatch matters for your use of ctstate, gate this UDP
 *     call on a packet counter instead of firing unconditionally on
 *     first reply. */
static __always_inline void ct_sync_v4(struct __sk_buff *skb, __be32 saddr, __be32 daddr,
                                        __be16 sport, __be16 dport, __u8 l4proto,
                                        __u8 set_assured)
{
    struct bpf_sock_tuple tuple = {};
    tuple.ipv4.saddr = saddr;
    tuple.ipv4.daddr = daddr;
    tuple.ipv4.sport = sport;
    tuple.ipv4.dport = dport;

    struct bpf_ct_opts opts = { .l4proto = l4proto, .netns_id = -1, .dir = IP_CT_DIR_ORIGINAL };
    struct nf_conn *ct = bpf_skb_ct_lookup(skb, &tuple, sizeof(tuple.ipv4), &opts, sizeof(opts));
    if (!ct)
        return; /* no kernel CT entry (yet) for this tuple -- fine, nothing to sync */

    __u32 status = IPS_SEEN_REPLY;
    if (set_assured)
        status |= IPS_ASSURED;
    bpf_ct_change_status(ct, status);
    bpf_ct_release(ct);
}

static __always_inline void ct_sync_v6(struct __sk_buff *skb, struct in6_addr *saddr, struct in6_addr *daddr,
                                        __be16 sport, __be16 dport, __u8 l4proto,
                                        __u8 set_assured)
{
    struct bpf_sock_tuple tuple = {};
    __builtin_memcpy(tuple.ipv6.saddr, saddr, sizeof(tuple.ipv6.saddr));
    __builtin_memcpy(tuple.ipv6.daddr, daddr, sizeof(tuple.ipv6.daddr));
    tuple.ipv6.sport = sport;
    tuple.ipv6.dport = dport;

    struct bpf_ct_opts opts = { .l4proto = l4proto, .netns_id = -1, .dir = IP_CT_DIR_ORIGINAL };
    struct nf_conn *ct = bpf_skb_ct_lookup(skb, &tuple, sizeof(tuple.ipv6), &opts, sizeof(opts));
    if (!ct)
        return;

    __u32 status = IPS_SEEN_REPLY;
    if (set_assured)
        status |= IPS_ASSURED;
    bpf_ct_change_status(ct, status);
    bpf_ct_release(ct);
}

/* --- TCP handling --- */

/* Sequence-number comparison that's safe across 32-bit wraparound --
 * the standard trick (RFC 1323 sec. 4.3-style): compare via signed
 * subtraction instead of a plain >=, so a seq that's wrapped around is
 * still correctly seen as "after" one from just before the wrap. */
static __always_inline __u8 seq_after_or_eq(__u32 a, __u32 b)
{
    return (__s32)(a - b) >= 0;
}

/* conntrack-modeled TCP FSM. is_fwd_dir: this packet's tuple matches
 * the entry's own recorded (first-seen) direction (1) or is the
 * opposite/reply direction (0). RST always wins and jumps straight to
 * CLOSE from any state, matching conntrack -- but only once its
 * sequence number has been sanity-checked; see below.
 *
 * has_seq/seq: whether this call has a real sequence number to work
 * with. False only for fragment-continuation calls (handle_v4_frag_cont/
 * handle_v6_frag_cont) -- a non-first fragment carries no TCP header at
 * all, so there's no seq to check or learn from; RST/FIN/SYN are always
 * 0 on those calls anyway (see flow_update), so has_seq only actually
 * gates the sequence-tracking logic below, never the RST path itself.
 *
 * RST spoofing: without this check, anyone able to put a single packet
 * on the wire with the right 5-tuple -- no on-path position, no need to
 * see any real traffic, no sequence-number guessing beyond blind luck --
 * could force an arbitrary live flow straight to CLOSE. Since this
 * program's entire purpose is keeping asymmetric routing correct via
 * RTS pinning, and CLOSE's GC timeout (tcpCloseTimeout, default 10s) is
 * vastly shorter than ESTABLISHED's (default 5 days), a single forged
 * RST can erase a flow's pinning almost immediately. fwd_seq_hi/
 * rev_seq_hi track the highest sequence number actually observed in
 * each direction; a RST is only honored if its own sequence number
 * falls within TCP_SEQ_SLACK of that -- not full RFC 5961 window
 * validation (no advertised-window tracking), just enough to reject a
 * blind/naive spoof (seq=0, random 32-bit garbage) cheaply. An
 * out-of-window RST is silently ignored for state-tracking purposes
 * here -- this program doesn't drop packets outside the redirect
 * decision anyway, so "ignore" just means "don't treat it as a real
 * close", not "drop it on the wire". */
static __always_inline void tcp_advance_state(struct flow_val *e, __u8 is_fwd_dir,
                                               __u8 is_syn, __u8 is_fin, __u8 is_rst,
                                               __u8 has_seq, __u32 seq, __u64 now)
{
    __u8 s = e->state;
    __u32 *hi = is_fwd_dir ? &e->fwd_seq_hi : &e->rev_seq_hi;
    __u8  seq_bit = is_fwd_dir ? TCP_SEQ_FWD_SET : TCP_SEQ_REV_SET;

    if (is_rst) {
        __u8 have_baseline = (e->seq_flags & seq_bit) != 0;
        if (has_seq && have_baseline && !seq_after_or_eq(seq, *hi - TCP_SEQ_SLACK))
            return; /* out-of-window: not honored as a real close */

        if (s != TCP_S_CLOSE) {
            e->state = TCP_S_CLOSE;
            e->state_since_ns = now;
        }
        return;
    }

    if (has_seq && (!(e->seq_flags & seq_bit) || seq_after_or_eq(seq, *hi))) {
        *hi = seq;
        e->seq_flags |= seq_bit;
    }

    if (is_fin)
        e->fin_flags |= is_fwd_dir ? TCP_FIN_FWD : TCP_FIN_REV;

    switch (s) {
    case TCP_S_SYN_SENT:
        if (!is_fwd_dir)
            s = TCP_S_SYN_RECV;
        break;
    case TCP_S_SYN_RECV:
        if (is_fwd_dir && !is_syn)
            s = TCP_S_ESTABLISHED;
        break;
    case TCP_S_ESTABLISHED:
        if (is_fin)
            s = TCP_S_FIN_WAIT;
        break;
    case TCP_S_FIN_WAIT:
    case TCP_S_CLOSE_WAIT:
        if ((e->fin_flags & TCP_FIN_FWD) && (e->fin_flags & TCP_FIN_REV))
            s = TCP_S_LAST_ACK;
        else if (s == TCP_S_FIN_WAIT && !is_fin)
            s = TCP_S_CLOSE_WAIT; /* the not-yet-FIN'd side acked the close */
        break;
    case TCP_S_LAST_ACK:
        s = TCP_S_TIME_WAIT;
        break;
    default: /* TCP_S_TIME_WAIT, TCP_S_CLOSE */
        break;
    }

    if (s != e->state) {
        e->state = s;
        e->state_since_ns = now;
    }
}

/* Unified flow-state update for the merged v4_flows/v6_flows map --
 * replaces the old tcp_flow_update/udp_flow_update/simple_flow_update
 * trio now that all four protocols share one map. The ifindex re-pin is
 * identical across every protocol (that's why those three used to be
 * near-duplicates of each other); proto then selects whichever
 * additional state a given protocol actually tracks. is_syn/is_fin/
 * is_rst/has_seq/seq are TCP-only; every other caller (UDP, ICMP,
 * generic, and every fragment-continuation call regardless of protocol)
 * passes 0 for all five -- flow_update itself only ever reads them when
 * proto == IPPROTO_TCP. */
static __always_inline void flow_update(struct flow_val *e, __u8 proto, __u8 is_fwd_dir,
                                         __u32 cur_ifindex, __u64 now,
                                         __u8 has_seq, __u32 seq,
                                         __u8 is_syn, __u8 is_fin, __u8 is_rst,
                                         __u8 is_ingress)
{
    e->last_seen_ns = now;
    /* Only a genuine arrival, on the flow's own forward direction, on
     * an RTS-tagged interface, may move the recorded redirect target --
     * see ct_redirect_ingress/egress for why is_ingress gates this. */
    if (is_fwd_dir && is_ingress && e->ifindex != cur_ifindex && rts_tagged(cur_ifindex))
        e->ifindex = cur_ifindex;

    if (proto == IPPROTO_TCP) {
        tcp_advance_state(e, is_fwd_dir, is_syn, is_fin, is_rst, has_seq, seq, now);
    } else if (proto == IPPROTO_UDP) {
        if (!is_fwd_dir && !e->assured)
            e->assured = 1; /* first reply-direction packet == assured */
    }
    /* ICMP echo / generic bucket: ifindex + last_seen_ns above is
     * everything they track -- nothing further to do. */
}

/* --- fragment tuple-completion cache ---
 *
 * Called only for the first fragment of a fragmented TCP/UDP/ICMP-echo
 * datagram, right after that packet has been looked up/inserted into
 * its real flow table same as any non-fragmented packet. It caches the
 * L4 fields a *later* fragment of the same datagram won't have (no L4
 * header at all past the first fragment) so that continuation fragments
 * can rebuild the same tuple key and go through the real flow table
 * themselves -- see handle_v4_frag_cont/handle_v6_frag_cont. Nothing
 * about the redirect decision is cached here; that's recomputed live,
 * every time, from whatever the flow table says right now.
 *
 * The generic bucket (no ports) deliberately never calls this: a later
 * fragment of a generic-proto datagram already has everything it needs
 * (address pair + proto) straight from its own IP/IPv6 header.
 *
 * IMPORTANT: sticky once created, same reasoning as before this
 * rewrite -- attached to both ingress and egress, so the first fragment
 * can hit this a second time on its own redirected egress path. An
 * unconditional overwrite there would race with a differently-ordered
 * duplicate/retransmitted first fragment. Existing entries are only
 * timestamp-refreshed, never replaced. Cache timeout mirrors
 * conntrack-adjacent defrag timeouts (see fragTimeout in main.go). */

static __always_inline void frag4_cache_tuple(struct iphdr *ip, __u8 is_first_frag,
                                               __be16 sport, __be16 dport, __u64 now)
{
    if (!is_first_frag)
        return;

    struct frag4_key fkey = { .saddr = ip->saddr, .daddr = ip->daddr,
                               .id = ip->id, .proto = ip->protocol };
    struct frag4_val *existing = bpf_map_lookup_elem(&frag4_track, &fkey);

    if (existing) {
        existing->last_seen_ns = now;
    } else {
        struct frag4_val v = { .proto = ip->protocol, .sport = sport, .dport = dport, .last_seen_ns = now };
        bpf_map_update_elem(&frag4_track, &fkey, &v, BPF_NOEXIST);
    }
}

static __always_inline void frag6_cache_tuple(struct in6_addr *saddr, struct in6_addr *daddr,
                                               __u8 proto, __be32 frag_id, __u8 is_first_frag,
                                               __be16 sport, __be16 dport, __u64 now)
{
    if (!is_first_frag)
        return;

    struct frag6_key fkey = { .saddr = *saddr, .daddr = *daddr, .id = frag_id, .proto = proto };
    struct frag6_val *existing = bpf_map_lookup_elem(&frag6_track, &fkey);

    if (existing) {
        existing->last_seen_ns = now;
    } else {
        struct frag6_val v = { .proto = proto, .sport = sport, .dport = dport, .last_seen_ns = now };
        bpf_map_update_elem(&frag6_track, &fkey, &v, BPF_NOEXIST);
    }
}

/* --- generic-bucket handling, shared ---
 *
 * Address pair + proto is already a complete key with no L4 header
 * involved (sport=dport=0 in v4_flow_key/v6_flow_key), so this same
 * helper serves three callers: a normal (non-fragment) generic packet,
 * the first fragment of a generic-proto datagram, and a later fragment
 * of one -- all three carry everything this needs straight from their
 * own IP/IPv6 header, no frag_track lookup required. */
static __always_inline int handle_v4_generic(struct iphdr *ip, __u32 cur_ifindex, __u8 is_ingress, __u64 now)
{
    struct v4_flow_key key = { .saddr = ip->saddr, .daddr = ip->daddr, .proto = ip->protocol };
    struct v4_flow_key rkey = { .saddr = ip->daddr, .daddr = ip->saddr, .proto = ip->protocol };

    struct flow_val *fwd = bpf_map_lookup_elem(&v4_flows, &key);
    struct flow_val *rev = bpf_map_lookup_elem(&v4_flows, &rkey);

    if (fwd) {
        flow_update(fwd, ip->protocol, 1, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
    } else if (rev) {
        flow_update(rev, ip->protocol, 0, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
    } else {
        struct flow_val v = { .ifindex = cur_ifindex, .last_seen_ns = now };
        bpf_map_update_elem(&v4_flows, &key, &v, BPF_NOEXIST);
    }

    return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));
}

static __always_inline int handle_v6_generic(struct ipv6hdr *ip6, __u8 proto, __u32 cur_ifindex, __u8 is_ingress, __u64 now)
{
    struct v6_flow_key key = { .saddr = ip6->saddr, .daddr = ip6->daddr, .proto = proto };
    struct v6_flow_key rkey = { .saddr = ip6->daddr, .daddr = ip6->saddr, .proto = proto };

    struct flow_val *fwd = bpf_map_lookup_elem(&v6_flows, &key);
    struct flow_val *rev = bpf_map_lookup_elem(&v6_flows, &rkey);

    if (fwd) {
        flow_update(fwd, proto, 1, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
    } else if (rev) {
        flow_update(rev, proto, 0, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
    } else {
        struct flow_val v = { .ifindex = cur_ifindex, .last_seen_ns = now };
        bpf_map_update_elem(&v6_flows, &key, &v, BPF_NOEXIST);
    }

    return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));
}

/* --- fragment continuation handling ---
 *
 * A non-first fragment carries no L4 header, so this rebuilds the same
 * v4_flow_key/v6_flow_key a normal packet would use from what
 * frag4_cache_tuple stashed on the first fragment, then runs it through
 * the *exact* same flow lookup + flow_update() + redirect_target() every
 * other packet uses -- this is a real flow packet now, not a special
 * case. Collapsing every protocol into one map means this no longer
 * needs to branch on fv->proto to pick a map at all -- fv->proto just
 * becomes part of the key, same as it is for a non-fragmented packet.
 * TCP flags are forced to 0 (no SYN/FIN/RST information exists past the
 * first fragment); a fragment can advance last_seen_ns and the
 * redirect-target ifindex re-pin, but never a TCP state transition or a
 * brand-new flow insert -- there's no header to safely infer either
 * from. If neither direction is already tracked (flow evicted, or we
 * simply never saw the first fragment -- reordering, map pressure,
 * etc.), there's nothing to attach this fragment to. */
static __always_inline int handle_v4_frag_cont(struct iphdr *ip, struct frag4_val *fv,
                                                __u32 cur_ifindex, __u8 is_ingress, __u64 now)
{
    struct v4_flow_key key = { .saddr = ip->saddr, .daddr = ip->daddr,
                                .sport = fv->sport, .dport = fv->dport, .proto = fv->proto };
    struct v4_flow_key rkey = { .saddr = ip->daddr, .daddr = ip->saddr,
                                 .sport = fv->dport, .dport = fv->sport, .proto = fv->proto };

    struct flow_val *fwd = bpf_map_lookup_elem(&v4_flows, &key);
    struct flow_val *rev = bpf_map_lookup_elem(&v4_flows, &rkey);

    if (fwd)
        flow_update(fwd, fv->proto, 1, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
    else if (rev)
        flow_update(rev, fv->proto, 0, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);

    return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));
}

static __always_inline int handle_v6_frag_cont(struct ipv6hdr *ip6, struct frag6_val *fv,
                                                __u32 cur_ifindex, __u8 is_ingress, __u64 now)
{
    struct v6_flow_key key = { .saddr = ip6->saddr, .daddr = ip6->daddr,
                                .sport = fv->sport, .dport = fv->dport, .proto = fv->proto };
    struct v6_flow_key rkey = { .saddr = ip6->daddr, .daddr = ip6->saddr,
                                 .sport = fv->dport, .dport = fv->sport, .proto = fv->proto };

    struct flow_val *fwd = bpf_map_lookup_elem(&v6_flows, &key);
    struct flow_val *rev = bpf_map_lookup_elem(&v6_flows, &rkey);

    if (fwd)
        flow_update(fwd, fv->proto, 1, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
    else if (rev)
        flow_update(rev, fv->proto, 0, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);

    return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));
}

/* --- v6 extension header walk --- */

struct v6_l4_info {
    void   *l4;
    __u8    proto;
    __u8    is_frag;
    __u8    is_first_frag;  /* offset==0: the L4 header follows right here
                              * in this packet -- true for both a genuine
                              * first-of-many fragment AND an RFC 6946
                              * "atomic" fragment (Fragment header present,
                              * offset==0, M==0 -- a complete datagram that
                              * just happens to carry a fragment header;
                              * see the more_frags comment). */
    __u8    more_frags;     /* the M bit: true only for a genuine first-of-
                              * many fragment. An atomic fragment has
                              * is_first_frag=1 but more_frags=0 -- it needs
                              * no reassembly tracking at all, since there's
                              * no second fragment ever coming; see how
                              * this gates frag6_cache_tuple in handle_v6. */
    __be32  frag_id;
};

/* Walks the v6 extension header chain starting at ip6->nexthdr to find
 * the real upper-layer header, skipping HOPOPTS/ROUTING/DSTOPTS
 * (standard 8*(hdrlen+1) TLV headers) and detecting FRAGMENT. AH/ESP/MH/
 * anything else stops the walk; out->proto/l4 then point at that
 * terminal header, unwrapped no further (generic bucket handles it,
 * same as any other opaque protocol). Bounded to IPV6_MAX_EXT_HDRS
 * iterations for the verifier. Returns -1 on a bounds check failure. */
static __always_inline int walk_v6_headers(struct ipv6hdr *ip6, void *data_end,
                                            struct v6_l4_info *out)
{
    void *cur = (void *)(ip6 + 1);
    __u8 nexthdr = ip6->nexthdr;

    out->is_frag = 0;
    out->is_first_frag = 0;
    out->more_frags = 0;
    out->frag_id = 0;

    #pragma unroll
    for (int i = 0; i < IPV6_MAX_EXT_HDRS; i++) {
        if (nexthdr == IPPROTO_HOPOPTS || nexthdr == IPPROTO_ROUTING || nexthdr == IPPROTO_DSTOPTS) {
            struct ipv6_opt_hdr *opt = cur;
            if ((void *)(opt + 1) > data_end)
                return -1;
            nexthdr = opt->nexthdr;
            cur = cur + (opt->hdrlen + 1) * 8;
            continue;
        }
        if (nexthdr == IPPROTO_FRAGMENT) {
            struct frag_hdr *fh = cur;
            if ((void *)(fh + 1) > data_end)
                return -1;
            __u16 fo = bpf_ntohs(fh->frag_off);
            out->is_frag = 1;
            /* offset==0 alone means the L4 header follows right here --
             * regardless of M. Requiring M too (the old formula) is
             * exactly the RFC 6946 atomic-fragment bug: a Fragment
             * header with offset=0 and M=0 is a complete, self-
             * contained datagram (some stacks emit these for PMTUD-
             * adjacent reasons), but the old check would misclassify it
             * as "a later fragment with no L4 header", sending it into
             * a frag6_track lookup that was never populated (nothing
             * ever caches for a packet that never took the first-
             * fragment path either) -- silent no-redirect, no tracking,
             * for a packet that was never actually split at all. */
            out->is_first_frag = (fo >> 3) == 0;
            out->more_frags = fo & 0x1;
            out->frag_id = fh->identification;
            nexthdr = fh->nexthdr;
            cur = cur + sizeof(*fh);
            if (!out->is_first_frag) {
                /* later fragment: no real L4 header follows in this
                 * packet at all -- stop here, caller must not
                 * dereference out->l4 in this case. */
                out->proto = nexthdr;
                out->l4 = cur;
                return 0;
            }
            continue; /* first fragment: real L4 header follows next */
        }
        break;
    }

    out->proto = nexthdr;
    out->l4 = cur;
    return 0;
}

/* --- broadcast/multicast bypass ---
 *
 * Neither ever has a single "reverse peer" a redirect target could mean
 * anything for -- mDNS/SSDP/DHCP-broadcast/etc. all have exactly one
 * sender and an entire subnet or link of listeners, so tracking them as
 * a flow is pure churn (map writes, LRU pressure) for something that
 * was never going to get redirected. Bypass early, before either one
 * ever touches a map. */
static __always_inline __u8 is_v4_mcast_or_bcast(__be32 daddr)
{
    __u32 d = bpf_ntohl(daddr);
    return (d >> 28) == 0xE          /* 224.0.0.0/4 multicast */
        || d == 0xFFFFFFFFu;         /* 255.255.255.255 limited broadcast --
                                       * subnet-directed broadcasts (e.g.
                                       * 10.0.0.255/24) aren't caught here,
                                       * that needs the subnet mask, which
                                       * this program doesn't have. */
}

static __always_inline __u8 is_v6_mcast(struct in6_addr *daddr)
{
    return ((__u8 *)daddr)[0] == 0xFF; /* ff00::/8 */
}

/* --- main per-family handlers --- */

static __always_inline int handle_v4(struct __sk_buff *skb, void *l3, void *data_end, __u32 cur_ifindex, __u8 is_ingress)
{
    struct iphdr *ip = l3;
    if ((void *)(ip + 1) > data_end)
        return TC_ACT_OK;

    if (is_v4_mcast_or_bcast(ip->daddr))
        return TC_ACT_OK;

    __u16 raw_frag = bpf_ntohs(ip->frag_off);
    __u8  mf = (raw_frag & 0x2000) != 0;
    __u16 frag_field = raw_frag & 0x1FFF;
    __u8  is_frag = mf || (frag_field != 0);
    __u8  is_first_frag = is_frag && (frag_field == 0); /* frag_field==0 && mf */

    __u64 now = bpf_ktime_get_ns();

    if (is_frag && !is_first_frag) {
        /* TCP/UDP/ICMP need ports/echo-id we don't have on this packet --
         * go find them via frag4_track. Anything else (generic bucket)
         * already has a complete key from this header alone. */
        if (ip->protocol == IPPROTO_TCP || ip->protocol == IPPROTO_UDP || ip->protocol == IPPROTO_ICMP) {
            struct frag4_key fkey = { .saddr = ip->saddr, .daddr = ip->daddr,
                                       .id = ip->id, .proto = ip->protocol };
            struct frag4_val *fv = bpf_map_lookup_elem(&frag4_track, &fkey);
            /* Never saw the first fragment (reordering, eviction, etc.) --
             * ICMP still has a valid fallback (generic, no ports needed);
             * TCP/UDP genuinely can't be attributed without ports. */
            if (fv)
                return handle_v4_frag_cont(ip, fv, cur_ifindex, is_ingress, now);
            if (ip->protocol == IPPROTO_ICMP)
                return handle_v4_generic(ip, cur_ifindex, is_ingress, now);
            return TC_ACT_OK;
        }
        return handle_v4_generic(ip, cur_ifindex, is_ingress, now);
    }

    void *l4 = (void *)ip + ip->ihl * 4;

    if (ip->protocol == IPPROTO_TCP) {
        struct tcphdr *tcp = l4;
        if ((void *)(tcp + 1) > data_end)
            return TC_ACT_OK;

        struct v4_flow_key key = { .saddr = ip->saddr, .daddr = ip->daddr, .sport = tcp->source, .dport = tcp->dest, .proto = IPPROTO_TCP };
        struct v4_flow_key rkey = { .saddr = ip->daddr, .daddr = ip->saddr, .sport = tcp->dest, .dport = tcp->source, .proto = IPPROTO_TCP };

        __u8 is_syn = tcp->syn, is_fin = tcp->fin, is_rst = tcp->rst;
        __u32 seq = bpf_ntohl(tcp->seq);

        struct flow_val *fwd = bpf_map_lookup_elem(&v4_flows, &key);
        struct flow_val *rev = bpf_map_lookup_elem(&v4_flows, &rkey);

        if (fwd) {
            flow_update(fwd, IPPROTO_TCP, 1, cur_ifindex, now, 1, seq, is_syn, is_fin, is_rst, is_ingress);
        } else if (rev) {
            flow_update(rev, IPPROTO_TCP, 0, cur_ifindex, now, 1, seq, is_syn, is_fin, is_rst, is_ingress);
            ct_sync_v4(skb, rkey.saddr, rkey.daddr, rkey.sport, rkey.dport, IPPROTO_TCP,
                       rev->state == TCP_S_ESTABLISHED);
        } else {
            struct flow_val v = {
                .ifindex = cur_ifindex,
                .state = is_rst ? TCP_S_CLOSE : is_syn ? TCP_S_SYN_SENT : is_fin ? TCP_S_FIN_WAIT : TCP_S_ESTABLISHED,
                .fin_flags = is_fin ? TCP_FIN_FWD : 0,
                .seq_flags = TCP_SEQ_FWD_SET, /* this packet, being the one that
                                                * created the flow, is definitionally
                                                * the forward direction. */
                .last_seen_ns = now,
                .state_since_ns = now,
                .fwd_seq_hi = seq,
            };
            bpf_map_update_elem(&v4_flows, &key, &v, BPF_NOEXIST);
        }

        frag4_cache_tuple(ip, is_first_frag, tcp->source, tcp->dest, now);
        return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));

    } else if (ip->protocol == IPPROTO_UDP) {
        struct udphdr *udp = l4;
        if ((void *)(udp + 1) > data_end)
            return TC_ACT_OK;

        struct v4_flow_key key = { .saddr = ip->saddr, .daddr = ip->daddr, .sport = udp->source, .dport = udp->dest, .proto = IPPROTO_UDP };
        struct v4_flow_key rkey = { .saddr = ip->daddr, .daddr = ip->saddr, .sport = udp->dest, .dport = udp->source, .proto = IPPROTO_UDP };

        struct flow_val *fwd = bpf_map_lookup_elem(&v4_flows, &key);
        struct flow_val *rev = bpf_map_lookup_elem(&v4_flows, &rkey);

        if (fwd) {
            flow_update(fwd, IPPROTO_UDP, 1, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
        } else if (rev) {
            flow_update(rev, IPPROTO_UDP, 0, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
            ct_sync_v4(skb, rkey.saddr, rkey.daddr, rkey.sport, rkey.dport, IPPROTO_UDP, 1);
        } else {
            struct flow_val v = { .ifindex = cur_ifindex, .assured = 0, .last_seen_ns = now };
            bpf_map_update_elem(&v4_flows, &key, &v, BPF_NOEXIST);
        }

        frag4_cache_tuple(ip, is_first_frag, udp->source, udp->dest, now);
        return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));

    } else if (ip->protocol == IPPROTO_ICMP) {
        struct icmphdr *icmp = l4;
        if ((void *)(icmp + 1) > data_end)
            return TC_ACT_OK;

        if (icmp->type == ICMP_ECHO || icmp->type == ICMP_ECHOREPLY) {
            /* echo id reuses the sport slot; dport left 0 -- same
             * proto-in-key trick as everything else in v4_flows. */
            struct v4_flow_key key = { .saddr = ip->saddr, .daddr = ip->daddr, .sport = icmp->un.echo.id, .proto = IPPROTO_ICMP };
            struct v4_flow_key rkey = { .saddr = ip->daddr, .daddr = ip->saddr, .sport = icmp->un.echo.id, .proto = IPPROTO_ICMP };

            struct flow_val *fwd = bpf_map_lookup_elem(&v4_flows, &key);
            struct flow_val *rev = bpf_map_lookup_elem(&v4_flows, &rkey);

            if (fwd) {
                flow_update(fwd, IPPROTO_ICMP, 1, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
            } else if (rev) {
                flow_update(rev, IPPROTO_ICMP, 0, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
            } else {
                struct flow_val v = { .ifindex = cur_ifindex, .last_seen_ns = now };
                bpf_map_update_elem(&v4_flows, &key, &v, BPF_NOEXIST);
            }

            frag4_cache_tuple(ip, is_first_frag, icmp->un.echo.id, 0, now);
            return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));
        }
        /* non-echo ICMP: falls through to generic below */
    }

    /* generic bucket: GRE, ESP, raw ipip, non-echo ICMP, anything else */
    return handle_v4_generic(ip, cur_ifindex, is_ingress, now);
}

static __always_inline int handle_v6(struct __sk_buff *skb, void *l3, void *data_end, __u32 cur_ifindex, __u8 is_ingress)
{
    struct ipv6hdr *ip6 = l3;
    if ((void *)(ip6 + 1) > data_end)
        return TC_ACT_OK;

    if (is_v6_mcast(&ip6->daddr))
        return TC_ACT_OK;

    struct v6_l4_info hdrs;
    if (walk_v6_headers(ip6, data_end, &hdrs) < 0)
        return TC_ACT_OK;

    __u64 now = bpf_ktime_get_ns();

    if (hdrs.is_frag && !hdrs.is_first_frag) {
        __u8 proto = hdrs.proto;
        if (proto == IPPROTO_TCP || proto == IPPROTO_UDP || proto == IPPROTO_ICMPV6) {
            struct frag6_key fkey = { .saddr = ip6->saddr, .daddr = ip6->daddr,
                                       .id = hdrs.frag_id, .proto = proto };
            struct frag6_val *fv = bpf_map_lookup_elem(&frag6_track, &fkey);
            if (fv)
                return handle_v6_frag_cont(ip6, fv, cur_ifindex, is_ingress, now);
            if (proto == IPPROTO_ICMPV6)
                return handle_v6_generic(ip6, proto, cur_ifindex, is_ingress, now);
            return TC_ACT_OK;
        }
        return handle_v6_generic(ip6, proto, cur_ifindex, is_ingress, now);
    }

    __u8 proto = hdrs.proto;
    void *l4 = hdrs.l4;
    /* Gated on more_frags too, not just is_frag/is_first_frag: an atomic
     * fragment (see the v6_l4_info comment) has is_first_frag=1 but
     * there's no second fragment ever coming for it, so caching a
     * tuple-completion entry for it in frag6_track would just be an
     * entry nothing will ever look up, sitting there until fragTimeout
     * reaps it. */
    __u8 is_first_frag = hdrs.is_frag && hdrs.is_first_frag && hdrs.more_frags;

    if (proto == IPPROTO_TCP) {
        struct tcphdr *tcp = l4;
        if ((void *)(tcp + 1) > data_end)
            return TC_ACT_OK;

        struct v6_flow_key key = { .saddr = ip6->saddr, .daddr = ip6->daddr, .sport = tcp->source, .dport = tcp->dest, .proto = IPPROTO_TCP };
        struct v6_flow_key rkey = { .saddr = ip6->daddr, .daddr = ip6->saddr, .sport = tcp->dest, .dport = tcp->source, .proto = IPPROTO_TCP };

        __u8 is_syn = tcp->syn, is_fin = tcp->fin, is_rst = tcp->rst;
        __u32 seq = bpf_ntohl(tcp->seq);

        struct flow_val *fwd = bpf_map_lookup_elem(&v6_flows, &key);
        struct flow_val *rev = bpf_map_lookup_elem(&v6_flows, &rkey);

        if (fwd) {
            flow_update(fwd, IPPROTO_TCP, 1, cur_ifindex, now, 1, seq, is_syn, is_fin, is_rst, is_ingress);
        } else if (rev) {
            flow_update(rev, IPPROTO_TCP, 0, cur_ifindex, now, 1, seq, is_syn, is_fin, is_rst, is_ingress);
            ct_sync_v6(skb, &rkey.saddr, &rkey.daddr, rkey.sport, rkey.dport, IPPROTO_TCP,
                       rev->state == TCP_S_ESTABLISHED);
        } else {
            struct flow_val v = {
                .ifindex = cur_ifindex,
                .state = is_rst ? TCP_S_CLOSE : is_syn ? TCP_S_SYN_SENT : is_fin ? TCP_S_FIN_WAIT : TCP_S_ESTABLISHED,
                .fin_flags = is_fin ? TCP_FIN_FWD : 0,
                .seq_flags = TCP_SEQ_FWD_SET,
                .last_seen_ns = now,
                .state_since_ns = now,
                .fwd_seq_hi = seq,
            };
            bpf_map_update_elem(&v6_flows, &key, &v, BPF_NOEXIST);
        }

        frag6_cache_tuple(&ip6->saddr, &ip6->daddr, proto, hdrs.frag_id, is_first_frag, tcp->source, tcp->dest, now);
        return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));

    } else if (proto == IPPROTO_UDP) {
        struct udphdr *udp = l4;
        if ((void *)(udp + 1) > data_end)
            return TC_ACT_OK;

        struct v6_flow_key key = { .saddr = ip6->saddr, .daddr = ip6->daddr, .sport = udp->source, .dport = udp->dest, .proto = IPPROTO_UDP };
        struct v6_flow_key rkey = { .saddr = ip6->daddr, .daddr = ip6->saddr, .sport = udp->dest, .dport = udp->source, .proto = IPPROTO_UDP };

        struct flow_val *fwd = bpf_map_lookup_elem(&v6_flows, &key);
        struct flow_val *rev = bpf_map_lookup_elem(&v6_flows, &rkey);

        if (fwd) {
            flow_update(fwd, IPPROTO_UDP, 1, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
        } else if (rev) {
            flow_update(rev, IPPROTO_UDP, 0, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
            ct_sync_v6(skb, &rkey.saddr, &rkey.daddr, rkey.sport, rkey.dport, IPPROTO_UDP, 1);
        } else {
            struct flow_val v = { .ifindex = cur_ifindex, .assured = 0, .last_seen_ns = now };
            bpf_map_update_elem(&v6_flows, &key, &v, BPF_NOEXIST);
        }

        frag6_cache_tuple(&ip6->saddr, &ip6->daddr, proto, hdrs.frag_id, is_first_frag, udp->source, udp->dest, now);
        return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));

    } else if (proto == IPPROTO_ICMPV6) {
        struct icmp6hdr *icmp6 = l4;
        if ((void *)(icmp6 + 1) > data_end)
            return TC_ACT_OK;

        if (icmp6->icmp6_type == ICMPV6_ECHO_REQUEST || icmp6->icmp6_type == ICMPV6_ECHO_REPLY) {
            __u16 id = icmp6->icmp6_dataun.u_echo.identifier;
            struct v6_flow_key key = { .saddr = ip6->saddr, .daddr = ip6->daddr, .sport = id, .proto = IPPROTO_ICMPV6 };
            struct v6_flow_key rkey = { .saddr = ip6->daddr, .daddr = ip6->saddr, .sport = id, .proto = IPPROTO_ICMPV6 };

            struct flow_val *fwd = bpf_map_lookup_elem(&v6_flows, &key);
            struct flow_val *rev = bpf_map_lookup_elem(&v6_flows, &rkey);

            if (fwd) {
                flow_update(fwd, IPPROTO_ICMPV6, 1, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
            } else if (rev) {
                flow_update(rev, IPPROTO_ICMPV6, 0, cur_ifindex, now, 0, 0, 0, 0, 0, is_ingress);
            } else {
                struct flow_val v = { .ifindex = cur_ifindex, .last_seen_ns = now };
                bpf_map_update_elem(&v6_flows, &key, &v, BPF_NOEXIST);
            }

            frag6_cache_tuple(&ip6->saddr, &ip6->daddr, proto, hdrs.frag_id, is_first_frag, id, 0, now);
            return do_redirect_or_ok(redirect_target(rev ? rev->ifindex : 0, cur_ifindex));
        }
        /* non-echo ICMPv6 (NDP/MLD/RS/RA/etc): falls through to
         * generic6 below -- not specially optimized. */
    }

    /* generic6 bucket */
    return handle_v6_generic(ip6, proto, cur_ifindex, is_ingress, now);
}

static __always_inline int handle(struct __sk_buff *skb, __u8 is_ingress)
{
    void *data     = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;
    __u32 cur_ifindex = skb->ifindex;

    void *l3;
    __be16 ethertype;
    __u8 l3only = bpf_map_lookup_elem(&l3_only_ifaces, &cur_ifindex) != NULL;

    if (l3only) {
        /* No ethhdr on pure-L3 tunnel devices -- peek the IP version
         * nibble instead, same as the kernel does for these. */
        struct iphdr *maybe_ip = data;
        if ((void *)(maybe_ip + 1) > data_end)
            return TC_ACT_OK;
        l3 = data;
        ethertype = (maybe_ip->version == 6) ? bpf_htons(ETH_P_IPV6) : bpf_htons(ETH_P_IP);
    } else {
        struct ethhdr *eth = data;
        if ((void *)(eth + 1) > data_end)
            return TC_ACT_OK;
        ethertype = eth->h_proto;
        l3 = eth + 1;
    }

    if (ethertype == bpf_htons(ETH_P_IP)) {
        return handle_v4(skb, l3, data_end, cur_ifindex, is_ingress);
    } else if (ethertype == bpf_htons(ETH_P_IPV6)) {
        return handle_v6(skb, l3, data_end, cur_ifindex, is_ingress);
    }
    return TC_ACT_OK;
}

/* ct_redirect is attached to BOTH tc ingress AND tc egress on every
 * interface. Necessary because a purely-local-origin flow (traffic
 * generated OR terminated by this box's own process, not a container
 * behind it) never has an ingress event for its own first packet in
 * that direction, only egress -- egress attachment is what lets such
 * flows get recorded AND get their reply force-redirected at all.
 *
 * That creates one hazard: a single FORWARDED packet (transiting the
 * box, not originating/terminating here) hits BOTH hooks in one trip --
 * ingress on arrival, then egress on the way back out -- and naive code
 * can mistake "this packet is LEAVING via interface X" for "this packet
 * just ARRIVED via interface X", corrupting the flow's recorded origin.
 *
 * is_ingress gates exactly ONE thing to prevent that: the forward-tuple
 * ifindex re-pin WRITE inside flow_update(). Only a genuine arrival may
 * move a flow's recorded origin. Two things are deliberately NOT gated
 * by is_ingress:
 *   - the first insert for a brand new flow (BPF_NOEXIST) -- must fire
 *     on whichever hook sees the flow first, to catch locally-
 *     originated flows.
 *   - the redirect decision itself, computed fresh via redirect_target()
 *     at the end of every branch (including the fragment-continuation
 *     ones) -- a locally-generated reply's only hook hit is egress too,
 *     and that's its one chance to be force-redirected onto the right
 *     interface. Safe to run on both hooks because the ifindex it reads
 *     is never corrupted by the write-side gate above. */
SEC("tc")
int ct_redirect_ingress(struct __sk_buff *skb)
{
    return handle(skb, 1);
}

SEC("tc")
int ct_redirect_egress(struct __sk_buff *skb)
{
    return handle(skb, 0);
}

char _license[] SEC("license") = "GPL";
