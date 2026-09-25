/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * melnode's forwarding datapath.
 *
 * Two producers (melnode_tun_xmit in melnode_core.c, and the tail of
 * melnode_handle_data there) each resolve the routing decision inline
 * (an RCU lookup, cheap and non-blocking) and hand off the actual I/O to
 * one of two per-destination delivery mechanisms owned by this file:
 *
 *  - Local-tun delivery: gro_cells_receive() on the destination tun's own
 *    GRO cells (melnode_tun_priv.gcells) - the same mechanism
 *    vxlan/geneve/gtp use to hand a decapsulated packet back into the
 *    stack without deep recursion. No custom queue.
 *  - Forward-onward (encrypt + send to a link): a bounded ptr_ring per
 *    link, drained by that link's own work_struct. Each queued item holds
 *    its own reference (kref) on the link, released by the consumer after
 *    processing it - not by whoever enqueued it - so a concurrent LINK_DEL
 *    can't free a link while items are still queued for it.
 *
 * Standing rule: no crypto call, no socket call, and no tun I/O ever
 * happens while holding a link lock or a table lock. Every lock-holding
 * section here only reads/copies fields and releases before doing I/O.
 */
#ifndef _MELNODE_ROUTING_H
#define _MELNODE_ROUTING_H

#include <linux/types.h>

struct melnode_link;

/* melnode's own routing header, living inside the decrypted Noise
 * transport payload - NOT part of melnode_messages.h, since that file is
 * the Noise/wire-framing layer ported from WireGuard, while this is
 * melnode-dp's own addition on top of it. See router.go/ctl.go: byte 0
 * proto, byte 1 src peer id, byte 2 dst peer id, byte 3 ttl. Shared
 * between melnode_core.c (which builds this header for tun-originated
 * traffic and parses it for RX dispatch) and this file (which needs the
 * TTL offset for the multi-hop decrement).
 */
#define MELNODE_HDR_LEN 4
#define MELNODE_HDR_OFF_PROTO 0
#define MELNODE_HDR_OFF_SRC 1
#define MELNODE_HDR_OFF_DST 2
#define MELNODE_HDR_OFF_TTL 3
#define MELNODE_DEFAULT_TTL 64

/* Sets up a freshly allocated link's kref/spinlock/outq/work_struct -
 * call once, before the link is published into melnode_dev.links[]
 * (LINK_ADD). Returns -ENOMEM if the outq can't be allocated.
 */
int melnode_routing_link_init(struct melnode_link *link);

/* RCU + kref lookup: the only safe way to reach a link struct now that
 * there's no device-wide lock. Every path that touches a link - RX index
 * lookup, the handshake-initiation pubkey scan, and INJECT - goes through
 * this. Returns NULL if there's no link at that peer id, or one is
 * mid-teardown. Caller must melnode_link_put() the result when done with
 * it.
 */
struct melnode_link *melnode_link_get(u8 peer_id);
void melnode_link_put(struct melnode_link *link);

/* The actual encrypt-and-UDP-send, given an already-kref'd link and a
 * plaintext buffer (melnode-header-prefixed, per MELNODE_HDR_* above).
 * Does not take ownership of `plain` - caller frees it. Shared code,
 * called two ways: synchronously from INJECT (melnode_core.c) and once
 * per queued item from a link's own outq_work. Returns 0, -ENOENT (no
 * valid session yet - caller should call melnode_send_initiation()), or
 * -EIO (send failed).
 */
int melnode_routing_send_now(struct melnode_link *link, const u8 *plain, size_t plain_len);

/* Resolves dst_peer_id via the route table and enqueues `plain` onto the
 * resolved next-hop link's outq. Always takes ownership of `plain` -
 * frees it on every path, including failure (no route, route points at a
 * link that no longer exists, or the link's queue is full). Does NOT bump
 * any stat itself - the two callers (melnode_tun_xmit, with
 * dst_peer_id = the tun's own fixed peer id, and the multi-hop branch of
 * melnode_routing_deliver_or_forward below) count failures under
 * different stats (TX_* vs RX_*), so each does its own accounting based
 * on the returned error.
 */
int melnode_routing_route_and_send(u8 dst_peer_id, u8 *plain, size_t plain_len);

/* proto == 0 (tunneled IP) dispatch for the UDP-ingress side, once
 * melnode_handle_data has already decrypted, length-checked (plain_len >=
 * MELNODE_HDR_LEN), and parsed the header. Delivers locally
 * (gro_cells_receive) if dst is us, otherwise decrements TTL and forwards
 * via melnode_routing_route_and_send. Always takes ownership of `plain`.
 * proto != 0 (PUNT, handled entirely in melnode_core.c) and INJECT
 * (direct link send, bypassing the route table by design per API.md's
 * "one link is literal") never reach this.
 */
void melnode_routing_deliver_or_forward(u8 src, u8 dst, u8 ttl, u8 *plain, size_t plain_len);

/* Implemented in melnode_core.c (it needs the Noise handshake-creation
 * calls and the device's own identity) - declared here since
 * melnode_routing.c's outq_work also needs to trigger a handshake on
 * -ENOENT from melnode_routing_send_now(). Caller must already hold a ref
 * on `link` (melnode_link_get()) for the duration of this call; this
 * function does not take or drop one itself.
 */
void melnode_send_initiation(struct melnode_link *link);

#endif /* _MELNODE_ROUTING_H */
