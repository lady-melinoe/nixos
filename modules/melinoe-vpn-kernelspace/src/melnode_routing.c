// SPDX-License-Identifier: GPL-2.0-only

#include <linux/slab.h>
#include <linux/rcupdate.h>
#include <linux/netdevice.h>
#include <linux/skbuff.h>
#include <linux/if_ether.h>
#include <linux/random.h>
#include <net/gro_cells.h>

#include "melnode_compat.h"
#include "melnode_core.h"
#include "melnode_index.h"
#include "melnode_routing.h"
#include "melnode_socket.h"
#include "melnode_transport.h"

struct melnode_routing_item {
	u8 *plain;
	size_t plain_len;
};

static void item_free(void *ptr)
{
	struct melnode_routing_item *item = ptr;

	kfree(item->plain);
	kfree(item);
}

static void link_release(struct kref *kref)
{
	struct melnode_link *link = container_of(kref, struct melnode_link, kref);

	ptr_ring_cleanup(&link->outq, item_free);
	memzero_explicit(&link->keypairs, sizeof(link->keypairs));
	memzero_explicit(&link->handshake, sizeof(link->handshake));
	memzero_explicit(&link->cookie, sizeof(link->cookie));
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

#define SECS_TO_NS(s) ((u64)(s) * NSEC_PER_SEC)

u64 melnode_rekey_jitter_ns(void)
{
	return (u64)get_random_u32_below(MELNODE_REKEY_JITTER_MAX_MS) * NSEC_PER_MSEC;
}

bool melnode_link_endpoint_take_locked(struct melnode_link *link, struct melnode_endpoint *out)
{
	if (!link->has_endpoint)
		return false;
	if (link->clear_src) {
		link->endpoint.has_src = false;
		link->endpoint.src_ifindex = 0;
		memset(&link->endpoint.src, 0, sizeof(link->endpoint.src));
		link->clear_src = false;
	}
	*out = link->endpoint;
	return true;
}

void melnode_link_set_endpoint_from_packet_locked(struct melnode_link *link,
						  const struct melnode_endpoint *ep)
{
	link->clear_src = false;
	link->endpoint = *ep;
	link->has_endpoint = true;
}

void melnode_link_set_endpoint_configured_locked(struct melnode_link *link, const void *addr,
						 int len)
{
	memset(&link->endpoint, 0, sizeof(link->endpoint));
	memcpy(link->endpoint.addr, addr, len);
	link->endpoint.addr_len = len;
	link->clear_src = false;
	link->has_endpoint = true;
}

void melnode_link_arm_timer_locked(struct melnode_link *link)
{
	u64 now = ktime_get_coarse_boottime_ns();
	u64 deadlines[] = { link->keepalive_at, link->new_handshake_at, link->retry_at,
			    link->wipe_at };
	u64 next = 0;
	int i;

	if (link->dead)
		return;

	for (i = 0; i < ARRAY_SIZE(deadlines); i++) {
		if (deadlines[i] && (!next || deadlines[i] < next))
			next = deadlines[i];
	}
	if (!next)
		return;

	kref_get(&link->kref);
	if (mod_delayed_work(MELNODE_TIMER_WQ, &link->timer,
			     next > now ? nsecs_to_jiffies64(next - now) + 1 : 0))
		melnode_link_put(link);
}

void melnode_link_session_established_locked(struct melnode_link *link, bool handshake_complete)
{
	if (handshake_complete) {
		link->retry_at = 0;
		link->handshake_attempts = 0;
		link->sent_last_minute_handshake = false;
	}
	link->new_handshake_at = 0;
	link->wipe_at = ktime_get_coarse_boottime_ns() + SECS_TO_NS(3 * MELNODE_REJECT_AFTER_TIME);
	melnode_link_arm_timer_locked(link);
}

void melnode_link_shutdown(struct melnode_link *link)
{
	spin_lock_bh(&link->lock);
	link->dead = true;
	melnode_index_sync(link);
	spin_unlock_bh(&link->lock);

	if (cancel_delayed_work_sync(&link->timer))
		melnode_link_put(link);
}

static bool deadline_due(u64 *deadline, u64 now)
{
	if (!*deadline || *deadline > now)
		return false;
	*deadline = 0;
	return true;
}

static void link_timer_fn(struct work_struct *work)
{
	struct melnode_link *link = container_of(to_delayed_work(work), struct melnode_link, timer);
	bool keepalive, initiate = false, retry = false;
	u64 now = ktime_get_coarse_boottime_ns();

	spin_lock_bh(&link->lock);
	if (link->dead) {
		spin_unlock_bh(&link->lock);
		goto out;
	}

	if (deadline_due(&link->wipe_at, now)) {
		melnode_noise_keypairs_clear(&link->keypairs);
		melnode_noise_handshake_clear(&link->handshake);
		melnode_index_sync(link);
		link->keepalive_at = 0;
		link->new_handshake_at = 0;
		link->retry_at = 0;
	}
	keepalive = deadline_due(&link->keepalive_at, now);
	if (deadline_due(&link->new_handshake_at, now)) {
		link->clear_src = true;
		initiate = true;
	}
	if (deadline_due(&link->retry_at, now)) {
		if (link->handshake_attempts > MELNODE_MAX_TIMER_HANDSHAKES) {
			link->keepalive_at = 0;
			keepalive = false;
			if (!link->wipe_at)
				link->wipe_at = now + SECS_TO_NS(3 * MELNODE_REJECT_AFTER_TIME);
		} else {
			link->handshake_attempts++;
			link->clear_src = true;
			retry = true;
		}
	}
	melnode_link_arm_timer_locked(link);
	spin_unlock_bh(&link->lock);

	if (keepalive) {
		melnode_routing_send_now(link, NULL, 0);
		spin_lock_bh(&link->lock);
		if (link->need_another_keepalive) {
			link->need_another_keepalive = false;
			link->keepalive_at = ktime_get_coarse_boottime_ns() +
					     SECS_TO_NS(MELNODE_KEEPALIVE_TIMEOUT);
			melnode_link_arm_timer_locked(link);
		}
		spin_unlock_bh(&link->lock);
	}
	if (initiate)
		melnode_send_initiation(link, false);
	else if (retry)
		melnode_send_initiation(link, true);
out:
	melnode_link_put(link);
}

static size_t melnode_padding_size(size_t len, u32 mtu)
{
	size_t last = len, padded;

	if (last > mtu)
		last %= mtu;
	padded = ALIGN(last, MELNODE_PADDING_MULTIPLE);
	if (padded > mtu)
		padded = mtu;
	return padded - last;
}

int melnode_routing_send_now(struct melnode_link *link, const u8 *plain, size_t plain_len)
{
	size_t pad = melnode_padding_size(plain_len, READ_ONCE(melnode_dev.mtu) + MELNODE_HDR_LEN);
	size_t enc_len = plain_len + pad;
	size_t out_len = sizeof(struct melnode_wire_data) + melnode_noise_encrypted_len(enc_len);
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	struct melnode_endpoint endpoint;
	struct melnode_wire_data *out;
	struct melnode_keypair *kp;
	__le32 remote_index;
	bool rekey;
	u64 counter;
	int ret = 0;

	out = kmalloc(out_len, GFP_KERNEL);
	if (!out)
		return -ENOMEM;

	spin_lock_bh(&link->lock);
	kp = &link->keypairs.current_kp;
	if (!link->has_endpoint || !kp->valid || !kp->sending.is_valid ||
	    kp->sending_counter >= MELNODE_REJECT_AFTER_MESSAGES ||
	    melnode_key_expired(&kp->sending, MELNODE_REJECT_AFTER_TIME)) {
		spin_unlock_bh(&link->lock);
		kfree(out);
		return -ENOENT;
	}
	rekey = kp->sending_counter >= MELNODE_REKEY_AFTER_MESSAGES ||
		(kp->initiator && melnode_key_expired(&kp->sending, MELNODE_REKEY_AFTER_TIME));
	counter = kp->sending_counter++;
	link->keepalive_at = 0;
	if (plain_len && !link->new_handshake_at) {
		link->new_handshake_at = ktime_get_coarse_boottime_ns() +
					 SECS_TO_NS(MELNODE_KEEPALIVE_TIMEOUT +
						    MELNODE_REKEY_TIMEOUT) +
					 melnode_rekey_jitter_ns();
		melnode_link_arm_timer_locked(link);
	}
	remote_index = kp->remote_index;
	memcpy(key, kp->sending.key, sizeof(key));
	melnode_link_endpoint_take_locked(link, &endpoint);
	spin_unlock_bh(&link->lock);

	out->header.type = cpu_to_le32(MELNODE_WIRE_MSG_DATA);
	out->receiver_index = remote_index;
	out->counter = cpu_to_le64(counter);
	if (plain_len)
		memcpy(out->encrypted_data, plain, plain_len);
	memset(out->encrypted_data + plain_len, 0, pad);
	melnode_transport_encrypt(out->encrypted_data, out->encrypted_data, enc_len, key, counter);
	memzero_explicit(key, sizeof(key));

	if (melnode_socket_send(&melnode_dev, out, out_len, &endpoint, 0) < 0)
		ret = -EIO;
	else
		atomic64_add(out_len, &link->tx_bytes);

	kfree(out);
	if (rekey)
		melnode_send_initiation(link, false);
	return ret;
}

static void outq_work_fn(struct work_struct *work)
{
	struct melnode_link *link = container_of(work, struct melnode_link, outq_work);
	struct melnode_routing_item *item;

	while ((item = ptr_ring_consume_bh(&link->outq))) {
		int err = melnode_routing_send_now(link, item->plain, item->plain_len);

		if (err == -ENOENT) {
			melnode_send_initiation(link, false);
			melnode_stat_inc(MELNODE_STAT_TX_NO_ROUTE);
		}
		item_free(item);
		cond_resched();
	}
	melnode_link_put(link);
}

int melnode_routing_link_init(struct melnode_link *link)
{
	kref_init(&link->kref);
	spin_lock_init(&link->lock);
	INIT_LIST_HEAD(&link->teardown_node);
	INIT_WORK(&link->outq_work, outq_work_fn);
	INIT_DELAYED_WORK(&link->timer, link_timer_fn);
	return ptr_ring_init(&link->outq, MELNODE_LINK_OUTQ_SIZE, GFP_KERNEL);
}

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
	item->plain = plain;
	item->plain_len = plain_len;

	if (ptr_ring_produce_bh(&link->outq, item)) {
		melnode_link_put(link);
		item_free(item);
		return -ENOSPC;
	}

	kref_get(&link->kref);
	if (!queue_work(melnode_wq, &link->outq_work))
		melnode_link_put(link);
	melnode_link_put(link);
	return 0;
}

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

	return send_via_link(nexthop, plain, plain_len);
}

static void deliver_local(u8 src, u8 *plain, size_t plain_len)
{
	size_t len = plain_len - MELNODE_HDR_LEN;
	const u8 *data = plain + MELNODE_HDR_LEN;
	struct melnode_tun_priv *priv;
	struct melnode_tun *tun;
	struct net_device *dev;
	struct sk_buff *skb;
	__be16 proto;

	if (!len) {
		melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
		goto out_free;
	}
	switch (data[0] >> 4) {
	case 4:
		proto = htons(ETH_P_IP);
		break;
	case 6:
		proto = htons(ETH_P_IPV6);
		break;
	default:
		melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
		goto out_free;
	}

	skb = alloc_skb(len, GFP_KERNEL);
	if (!skb) {
		melnode_stat_inc(MELNODE_STAT_RX_QUEUE_FULL);
		goto out_free;
	}
	skb_put_data(skb, data, len);
	skb->protocol = proto;
	skb_reset_network_header(skb);

	rcu_read_lock();
	tun = rcu_dereference(melnode_dev.tuns[src]);
	if (!tun || READ_ONCE(tun->state) != MELNODE_TUN_STARTED) {
		rcu_read_unlock();
		kfree_skb(skb);
		melnode_stat_inc(MELNODE_STAT_RX_NO_TUN);
		goto out_free;
	}
	dev = tun->dev;
	skb->dev = dev;
	priv = netdev_priv(dev);

	local_bh_disable();
	if (melnode_compat_gro_receive(&priv->gcells, skb) == NET_RX_DROP) {
		melnode_stat_inc(MELNODE_STAT_RX_TUN_FULL);
		DEV_STATS_INC(dev, rx_dropped);
	} else {
		DEV_STATS_INC(dev, rx_packets);
		DEV_STATS_ADD(dev, rx_bytes, len);
	}
	local_bh_enable();
	rcu_read_unlock();

out_free:
	kfree(plain);
}

static bool trim_ip(size_t *plain_len, const u8 *plain)
{
	size_t ip_len = *plain_len - MELNODE_HDR_LEN;
	const u8 *ip = plain + MELNODE_HDR_LEN;
	size_t length;

	if (ip_len >= 20 && (ip[0] >> 4) == 4) {
		length = get_unaligned_be16(ip + 2);
		if (length > ip_len || length < 20)
			return false;
	} else if (ip_len >= 40 && (ip[0] >> 4) == 6) {
		length = get_unaligned_be16(ip + 4) + 40;
		if (length > ip_len)
			return false;
	} else {
		return false;
	}
	*plain_len = MELNODE_HDR_LEN + length;
	return true;
}

void melnode_routing_deliver_or_forward(u8 src, u8 dst, u8 ttl, u8 *plain, size_t plain_len)
{
	int err;

	if (!trim_ip(&plain_len, plain)) {
		kfree(plain);
		melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
		return;
	}

	if (dst == READ_ONCE(melnode_dev.local_id)) {
		deliver_local(src, plain, plain_len);
		return;
	}

	if (ttl <= 1) {
		kfree(plain);
		melnode_stat_inc(MELNODE_STAT_RX_TTL_EXPIRED);
		return;
	}
	plain[MELNODE_HDR_OFF_TTL] = ttl - 1;

	err = melnode_routing_route_and_send(dst, plain, plain_len);
	if (err)
		melnode_stat_inc(err == -ENOSPC ? MELNODE_STAT_RX_QUEUE_FULL :
						  MELNODE_STAT_RX_NO_ROUTE);
}
