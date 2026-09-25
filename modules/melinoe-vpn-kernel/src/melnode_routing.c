// SPDX-License-Identifier: GPL-2.0-only
/*
 * melnode's forwarding datapath - see melnode_routing.h for the design.
 * This file owns:
 *  - the RCU+kref lookup that's the only safe way to reach a link struct
 *    (melnode_link_get/put);
 *  - the shared encrypt-and-send body (melnode_routing_send_now), called
 *    both synchronously (INJECT) and from a link's own queue worker;
 *  - the route-table lookup + per-link outbound ptr_ring/work_struct
 *    (melnode_routing_route_and_send);
 *  - the UDP-ingress local-vs-multi-hop dispatch
 *    (melnode_routing_deliver_or_forward), using gro_cells for local
 *    delivery instead of a hand-rolled queue.
 */

#include <linux/slab.h>
#include <linux/rcupdate.h>
#include <linux/netdevice.h>
#include <linux/skbuff.h>
#include <linux/if_ether.h>
#include <net/gro_cells.h>

#include "melnode_core.h"
#include "melnode_routing.h"
#include "melnode_socket.h"
#include "melnode_transport.h"

/* One forward-onward (encrypt + send) item queued on a link's outq. Small
 * and separately allocated; `plain` is a distinct, already-owned
 * allocation (built by the producer - melnode_tun_xmit or
 * melnode_routing_deliver_or_forward's multi-hop branch) whose ownership
 * transfers to this item.
 */
struct melnode_routing_item {
	struct melnode_link *link; /* ref held; released by outq_work_fn */
	u8 *plain;
	size_t plain_len;
};

static void link_release(struct kref *kref)
{
	struct melnode_link *link = container_of(kref, struct melnode_link, kref);

	/* By the time kref hits zero, nothing can still be enqueuing into or
	 * consuming from outq: kref_get_unless_zero() (melnode_link_get())
	 * can't succeed once the count is provably zero, and every existing
	 * holder (the table's own ref, dropped by LINK_DEL, and each queued
	 * item's ref, dropped by outq_work_fn after processing it) must have
	 * already let go for this callback to run at all. Safe to clean up
	 * synchronously here.
	 */
	ptr_ring_cleanup(&link->outq, NULL);
	kfree_rcu(link, rcu);
}

struct melnode_link *melnode_link_get(u8 peer_id)
{
	struct melnode_link *link;

	rcu_read_lock();
	link = rcu_dereference(melnode_dev.links[peer_id]);
	if (link && !kref_get_unless_zero(&link->kref))
		link = NULL;
	rcu_read_unlock();
	return link;
}

void melnode_link_put(struct melnode_link *link)
{
	kref_put(&link->kref, link_release);
}

int melnode_routing_send_now(struct melnode_link *link, const u8 *plain, size_t plain_len)
{
	struct melnode_keypair kp_copy;
	u8 endpoint[sizeof(struct sockaddr_in6)];
	int endpoint_len;
	struct melnode_wire_data *out;
	size_t out_len = sizeof(*out) + melnode_noise_encrypted_len(plain_len);
	u64 counter;
	int ret = 0;

	spin_lock_bh(&link->lock);
	if (!link->has_endpoint || !link->keypairs.current_kp.valid ||
	    !link->keypairs.current_kp.sending.is_valid) {
		spin_unlock_bh(&link->lock);
		return -ENOENT;
	}
	kp_copy = link->keypairs.current_kp; /* snapshot key+index; counter is atomic */
	endpoint_len = link->endpoint_len;
	memcpy(endpoint, link->endpoint, endpoint_len);
	spin_unlock_bh(&link->lock);

	out = kmalloc(out_len, GFP_ATOMIC);
	if (!out)
		return -ENOMEM;
	out->header.type = cpu_to_le32(MELNODE_WIRE_MSG_DATA);
	out->receiver_index = kp_copy.remote_index;
	/* Encrypt using the *live* counter (link->keypairs.current_kp is the
	 * real keypair, not a copy - kp_copy is only for the fields that
	 * don't need to be live: remote_index and the validity check above).
	 */
	melnode_transport_encrypt(out->encrypted_data, plain, plain_len, &link->keypairs.current_kp,
				   &counter);
	out->counter = cpu_to_le64(counter);

	if (melnode_socket_send(&melnode_dev, out, out_len, endpoint, endpoint_len) < 0)
		ret = -EIO;
	else
		atomic64_add(plain_len, &link->tx_bytes);

	kfree(out);
	return ret;
}

static void outq_work_fn(struct work_struct *work)
{
	struct melnode_link *link = container_of(work, struct melnode_link, outq_work);
	struct melnode_routing_item *item;

	while ((item = ptr_ring_consume_bh(&link->outq))) {
		int err = melnode_routing_send_now(link, item->plain, item->plain_len);

		if (err == -ENOENT) {
			/* No valid session yet. Originally (single synchronous
			 * send path) this was discovered and counted at xmit
			 * time; now it only surfaces here, once the consumer
			 * actually tries to send - same stat as before
			 * (MELNODE_STAT_TX_QUEUE_FULL: no staged-packet queue,
			 * see melnode_core.c's file header), just counted later.
			 */
			melnode_send_initiation(link);
			melnode_stat_inc(MELNODE_STAT_TX_QUEUE_FULL);
		}
		kfree(item->plain);
		/* Release this item's own reference last: melnode_send_initiation()
		 * above needs `link` to still be guaranteed alive. Touching
		 * `link`/`link->outq` again on the next loop iteration, even if
		 * this put was the very last reference, is safe: kref's release
		 * callback (link_release, above) only schedules a deferred,
		 * call_rcu-based free - it can never synchronously free memory
		 * still being touched by the same, uninterrupted call stack that
		 * just dropped the last reference.
		 */
		melnode_link_put(link);
		kfree(item);
	}
}

int melnode_routing_link_init(struct melnode_link *link)
{
	kref_init(&link->kref);
	spin_lock_init(&link->lock);
	INIT_WORK(&link->outq_work, outq_work_fn);
	return ptr_ring_init(&link->outq, MELNODE_LINK_OUTQ_SIZE, GFP_KERNEL);
}

/* Enqueues `plain` (ownership transferred) onto `link_peer_id`'s outq.
 * Always resolves `plain`'s fate - frees it on every path. Internal to
 * this file: melnode_routing_route_and_send() is the public entry point,
 * which resolves link_peer_id via the route table first.
 */
static int send_via_link(u8 link_peer_id, u8 *plain, size_t plain_len)
{
	struct melnode_link *link;
	struct melnode_routing_item *item;

	link = melnode_link_get(link_peer_id);
	if (!link) {
		kfree(plain);
		return -ENOLINK;
	}

	item = kmalloc(sizeof(*item), GFP_ATOMIC);
	if (!item) {
		melnode_link_put(link);
		kfree(plain);
		return -ENOMEM;
	}
	item->link = link; /* ref transferred to the item - released in outq_work_fn */
	item->plain = plain;
	item->plain_len = plain_len;

	/* Ordering rule: produce first, then schedule_work() unconditionally -
	 * never "only if the ring looked empty", which is a lost-wakeup race
	 * (an item can land right after the worker decided the ring was empty
	 * and was about to exit, but before it re-checks).
	 */
	if (ptr_ring_produce_bh(&link->outq, item)) {
		melnode_link_put(link);
		kfree(plain);
		kfree(item);
		return -ENOSPC;
	}
	schedule_work(&link->outq_work);
	return 0;
}

/* Deliberately does NOT bump any stat itself: the two callers (tun xmit
 * and the RX-side multi-hop forward, below) count failures under
 * different stats (MELNODE_STAT_TX_* vs MELNODE_STAT_RX_*, matching the
 * original code's own split) - see each call site.
 */
int melnode_routing_route_and_send(u8 dst_peer_id, u8 *plain, size_t plain_len)
{
	struct melnode_route *route;
	u8 nexthop;

	rcu_read_lock();
	route = rcu_dereference(melnode_dev.routes[dst_peer_id]);
	if (!route) {
		rcu_read_unlock();
		kfree(plain);
		return -ENODEV;
	}
	nexthop = route->nexthop;
	rcu_read_unlock();

	return send_via_link(nexthop, plain, plain_len); /* -ENOLINK / -ENOMEM / -ENOSPC / 0 */
}

/* Local delivery for an already-decrypted, header-parsed proto==0
 * payload whose dst is us: hands the inner IP packet to `src`'s tun via
 * gro_cells - see melnode_routing.h's header comment. Always resolves
 * `plain`'s fate.
 */
static void deliver_local(u8 src, u8 *plain, size_t plain_len)
{
	struct melnode_tun *tun;
	struct melnode_tun_priv *priv;
	struct net_device *dev;
	struct sk_buff *skb;

	rcu_read_lock();
	tun = rcu_dereference(melnode_dev.tuns[src]);
	if (!tun || READ_ONCE(tun->state) != MELNODE_TUN_STARTED) {
		rcu_read_unlock();
		kfree(plain);
		melnode_stat_inc(MELNODE_STAT_RX_NO_TUN);
		return;
	}
	dev = tun->dev;
	dev_hold(dev); /* RCU alone protects the table's pointer, not the
			 * net_device itself - see melnode_core.h.
			 */
	rcu_read_unlock();

	skb = netdev_alloc_skb(dev, plain_len - MELNODE_HDR_LEN);
	if (!skb) {
		dev_put(dev);
		kfree(plain);
		melnode_stat_inc(MELNODE_STAT_RX_QUEUE_FULL);
		return;
	}
	skb_put_data(skb, plain + MELNODE_HDR_LEN, plain_len - MELNODE_HDR_LEN);
	skb->protocol = (plain[MELNODE_HDR_LEN] >> 4) == 6 ? htons(ETH_P_IPV6) : htons(ETH_P_IP);
	skb_reset_network_header(skb);
	skb->dev = dev;

	priv = netdev_priv(dev);
	gro_cells_receive(&priv->gcells, skb);

	dev_put(dev);
	kfree(plain);
}

void melnode_routing_deliver_or_forward(u8 src, u8 dst, u8 ttl, u8 *plain, size_t plain_len)
{
	if ((u32)dst == READ_ONCE(melnode_dev.local_id)) {
		deliver_local(src, plain, plain_len);
		return;
	}

	if (ttl <= 1) {
		kfree(plain);
		melnode_stat_inc(MELNODE_STAT_RX_TTL_EXPIRED);
		return;
	}
	plain[MELNODE_HDR_OFF_TTL] = ttl - 1;
	{
		int err = melnode_routing_route_and_send(dst, plain, plain_len);

		if (err)
			melnode_stat_inc(err == -ENOSPC ? MELNODE_STAT_RX_QUEUE_FULL :
							   MELNODE_STAT_RX_NO_ROUTE);
	}
}
