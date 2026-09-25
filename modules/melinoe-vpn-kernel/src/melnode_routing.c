// SPDX-License-Identifier: GPL-2.0-only

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

struct melnode_routing_item {
	struct melnode_link *link;
	u8 *plain;
	size_t plain_len;
};

static void link_release(struct kref *kref)
{
	struct melnode_link *link = container_of(kref, struct melnode_link, kref);

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
	u8 endpoint[sizeof(struct sockaddr_in6)];
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	size_t out_len = sizeof(struct melnode_wire_data) + melnode_noise_encrypted_len(plain_len);
	struct melnode_wire_data *out;
	struct melnode_keypair *kp;
	__le32 remote_index;
	int endpoint_len;
	u64 counter;
	int ret = 0;

	out = kmalloc(out_len, GFP_KERNEL);
	if (!out)
		return -ENOMEM;

	spin_lock_bh(&link->lock);
	kp = &link->keypairs.current_kp;
	if (!link->has_endpoint || !kp->valid || !kp->sending.is_valid ||
	    kp->sending_counter >= MELNODE_REJECT_AFTER_MESSAGES) {
		spin_unlock_bh(&link->lock);
		kfree(out);
		return -ENOENT;
	}
	counter = kp->sending_counter++;
	remote_index = kp->remote_index;
	memcpy(key, kp->sending.key, sizeof(key));
	endpoint_len = link->endpoint_len;
	memcpy(endpoint, link->endpoint, endpoint_len);
	spin_unlock_bh(&link->lock);

	out->header.type = cpu_to_le32(MELNODE_WIRE_MSG_DATA);
	out->receiver_index = remote_index;
	out->counter = cpu_to_le64(counter);
	melnode_transport_encrypt(out->encrypted_data, plain, plain_len, key, counter);
	memzero_explicit(key, sizeof(key));

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
			melnode_send_initiation(link);
			melnode_stat_inc(MELNODE_STAT_TX_QUEUE_FULL);
		}
		kfree(item->plain);
		melnode_link_put(link);
		kfree(item);
		cond_resched();
	}
}

int melnode_routing_link_init(struct melnode_link *link)
{
	kref_init(&link->kref);
	spin_lock_init(&link->lock);
	INIT_LIST_HEAD(&link->teardown_node);
	INIT_WORK(&link->outq_work, outq_work_fn);
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
	item->link = link;
	item->plain = plain;
	item->plain_len = plain_len;

	if (ptr_ring_produce_bh(&link->outq, item)) {
		melnode_link_put(link);
		kfree(plain);
		kfree(item);
		return -ENOSPC;
	}
	schedule_work(&link->outq_work);
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

	rcu_read_lock();
	tun = rcu_dereference(melnode_dev.tuns[src]);
	if (!tun || READ_ONCE(tun->state) != MELNODE_TUN_STARTED) {
		rcu_read_unlock();
		melnode_stat_inc(MELNODE_STAT_RX_NO_TUN);
		goto out_free;
	}
	dev = tun->dev;
	dev_hold(dev);
	rcu_read_unlock();

	skb = netdev_alloc_skb(dev, len);
	if (!skb) {
		dev_put(dev);
		melnode_stat_inc(MELNODE_STAT_RX_QUEUE_FULL);
		goto out_free;
	}
	skb_put_data(skb, data, len);
	skb->protocol = proto;
	skb_reset_network_header(skb);
	skb->dev = dev;

	priv = netdev_priv(dev);
	local_bh_disable();
	if (gro_cells_receive(&priv->gcells, skb) == NET_RX_DROP) {
		melnode_stat_inc(MELNODE_STAT_RX_QUEUE_FULL);
		DEV_STATS_INC(dev, rx_dropped);
	} else {
		DEV_STATS_INC(dev, rx_packets);
		DEV_STATS_ADD(dev, rx_bytes, len);
	}
	local_bh_enable();

	dev_put(dev);
out_free:
	kfree(plain);
}

void melnode_routing_deliver_or_forward(u8 src, u8 dst, u8 ttl, u8 *plain, size_t plain_len)
{
	int err;

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
