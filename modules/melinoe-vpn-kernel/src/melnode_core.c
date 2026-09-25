// SPDX-License-Identifier: GPL-2.0-only

#include <linux/module.h>
#include <linux/init.h>
#include <linux/mutex.h>
#include <linux/ktime.h>
#include <linux/netlink.h>
#include <linux/in.h>
#include <linux/in6.h>
#include <linux/if.h>
#include <linux/if_arp.h>
#include <linux/if_ether.h>
#include <linux/list.h>
#include <linux/netdevice.h>
#include <linux/rtnetlink.h>
#include <linux/rcupdate.h>
#include <net/genetlink.h>
#include <net/gro_cells.h>
#include <crypto/curve25519.h>
#include <crypto/chacha20poly1305.h>

#include "melnode_core.h"
#include "melnode_routing.h"
#include "melnode_socket.h"
#include "melnode_transport.h"
#include "melnode_ratelimiter.h"

#define MELNODE_A_MAX MELNODE_A_EVT_TIME
#define MELNODE_TUN_TX_QUEUE_LEN 500

struct melnode_device melnode_dev = {
	.config_lock = __MUTEX_INITIALIZER(melnode_dev.config_lock),
	.tables_mutex = __MUTEX_INITIALIZER(melnode_dev.tables_mutex),
	.key_lock = __SPIN_LOCK_UNLOCKED(melnode_dev.key_lock),
	.attach_lock = __SPIN_LOCK_UNLOCKED(melnode_dev.attach_lock),
	.pending_teardown_lock = __SPIN_LOCK_UNLOCKED(melnode_dev.pending_teardown_lock),
	.pending_teardown = LIST_HEAD_INIT(melnode_dev.pending_teardown),
};

struct melnode_pending_teardown {
	struct list_head list;
	struct net_device *dev;
};

static struct genl_family melnode_genl_family;

static struct genl_multicast_group melnode_mcgrps[] = {
	[0] = { .name = "events" },
};

static const struct nla_policy melnode_policy[MELNODE_A_MAX + 1] = {
	[MELNODE_A_API_VERSION] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_LOCAL_ID] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PRIVATE_KEY] = NLA_POLICY_EXACT_LEN(32),
	[MELNODE_A_LISTEN_PORT] = NLA_POLICY_EXACT_LEN(sizeof(u16)),
	[MELNODE_A_FWMARK] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_MTU] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PEER_ID] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PUBLIC_KEY] = NLA_POLICY_EXACT_LEN(32),
	[MELNODE_A_ENDPOINT] = NLA_POLICY_RANGE(NLA_BINARY, sizeof(struct sockaddr_in),
						sizeof(struct sockaddr_in6)),
	[MELNODE_A_ROUTE_DST] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_ROUTE_NEXTHOP] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PKT_LINK] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PKT_PROTO] = NLA_POLICY_EXACT_LEN(sizeof(u8)),
	[MELNODE_A_PKT_DST] = NLA_POLICY_EXACT_LEN(sizeof(u8)),
	[MELNODE_A_PKT_TTL] = NLA_POLICY_EXACT_LEN(sizeof(u8)),
	[MELNODE_A_PKT_DATA] = { .type = NLA_BINARY },
	[MELNODE_A_TUN_NAME] = { .type = NLA_NUL_STRING, .len = IFNAMSIZ - 1 },
};

static void melnode_tun_teardown_work_fn(struct work_struct *work)
{
	struct melnode_pending_teardown *p, *tmp;
	LIST_HEAD(local);
	LIST_HEAD(dead);

	spin_lock_bh(&melnode_dev.pending_teardown_lock);
	list_splice_init(&melnode_dev.pending_teardown, &local);
	spin_unlock_bh(&melnode_dev.pending_teardown_lock);

	if (list_empty(&local))
		return;

	rtnl_lock();
	list_for_each_entry(p, &local, list)
		unregister_netdevice_queue(p->dev, &dead);
	unregister_netdevice_many(&dead);
	rtnl_unlock();

	list_for_each_entry_safe(p, tmp, &local, list) {
		list_del(&p->list);
		kfree(p);
	}
}

static void melnode_queue_tun_teardown(struct net_device *dev)
{
	struct melnode_pending_teardown *p;

	p = kmalloc(sizeof(*p), GFP_KERNEL);
	if (!p) {
		rtnl_lock();
		unregister_netdevice(dev);
		rtnl_unlock();
		return;
	}
	p->dev = dev;
	spin_lock_bh(&melnode_dev.pending_teardown_lock);
	list_add_tail(&p->list, &melnode_dev.pending_teardown);
	spin_unlock_bh(&melnode_dev.pending_teardown_lock);
	schedule_work(&melnode_dev.tun_teardown_work);
}

void melnode_stat_inc(enum melnode_stat id)
{
	if (id > 0 && id <= MELNODE_STAT_RX_QUEUE_FULL)
		atomic64_inc(&melnode_dev.stats[id]);
}

void melnode_get_keys(u8 public_key[32], u8 private_key[32])
{
	spin_lock_bh(&melnode_dev.key_lock);
	if (public_key)
		memcpy(public_key, melnode_dev.public_key, 32);
	if (private_key)
		memcpy(private_key, melnode_dev.private_key, 32);
	spin_unlock_bh(&melnode_dev.key_lock);
}

static bool melnode_configured(void)
{
	return READ_ONCE(melnode_dev.configured);
}

static int melnode_get_u8_id(struct nlattr *attr, u8 *out)
{
	u32 v;

	if (!attr)
		return -EINVAL;
	v = nla_get_u32(attr);
	if (v > 255)
		return -EINVAL;
	*out = v;
	return 0;
}

static u32 melnode_gen_index(u8 peer_id)
{
	u32 r;

	get_random_bytes(&r, sizeof(r));
	return (r & ~(u32)0xff) | peer_id;
}

static u8 melnode_index_peer_id(__le32 index)
{
	return le32_to_cpu(index) & 0xff;
}

static int melnode_endpoint_len(const void *data, int len)
{
	const struct sockaddr *sa = data;

	if (sa->sa_family == AF_INET && len >= sizeof(struct sockaddr_in))
		return sizeof(struct sockaddr_in);
	if (sa->sa_family == AF_INET6 && len >= sizeof(struct sockaddr_in6))
		return sizeof(struct sockaddr_in6);
	return -EINVAL;
}

static void melnode_link_set_endpoint(struct melnode_link *link, const void *addr, int len)
{
	memcpy(link->endpoint, addr, len);
	link->endpoint_len = len;
	link->has_endpoint = true;
}

static int melnode_nl_hello(struct sk_buff *skb, struct genl_info *info)
{
	struct sk_buff *reply;
	void *hdr;

	if (!info->attrs[MELNODE_A_API_VERSION])
		return -EINVAL;
	if (nla_get_u32(info->attrs[MELNODE_A_API_VERSION]) != MELNODE_GENL_VERSION)
		return -EOPNOTSUPP;

	reply = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_KERNEL);
	if (!reply)
		return -ENOMEM;

	hdr = genlmsg_put_reply(reply, info, &melnode_genl_family, 0, MELNODE_CMD_HELLO);
	if (!hdr ||
	    nla_put_u32(reply, MELNODE_A_API_VERSION, MELNODE_GENL_VERSION) ||
	    nla_put_u8(reply, MELNODE_A_CONFIGURED, melnode_configured())) {
		nlmsg_free(reply);
		return -EMSGSIZE;
	}

	genlmsg_end(reply, hdr);
	return genlmsg_reply(reply, info);
}

static int melnode_nl_attach(struct sk_buff *skb, struct genl_info *info)
{
	spin_lock_bh(&melnode_dev.attach_lock);
	melnode_dev.attached_portid = info->snd_portid;
	spin_unlock_bh(&melnode_dev.attach_lock);
	return 0;
}

static int melnode_netlink_notifier_call(struct notifier_block *nb, unsigned long state,
					 void *_notify)
{
	struct netlink_notify *notify = _notify;

	if (state != NETLINK_URELEASE || notify->protocol != NETLINK_GENERIC)
		return NOTIFY_DONE;

	spin_lock_bh(&melnode_dev.attach_lock);
	if (melnode_dev.attached_portid == notify->portid)
		melnode_dev.attached_portid = 0;
	spin_unlock_bh(&melnode_dev.attach_lock);
	return NOTIFY_DONE;
}

static struct notifier_block melnode_netlink_notifier = {
	.notifier_call = melnode_netlink_notifier_call,
};

static void melnode_send_punt(u8 ingress_link, u8 proto, u8 src, u8 dst, u8 ttl, const u8 *payload,
			      size_t payload_len)
{
	struct sk_buff *skb;
	void *hdr;
	u32 portid;

	portid = READ_ONCE(melnode_dev.attached_portid);
	if (!portid)
		goto dropped;

	skb = genlmsg_new(NLMSG_DEFAULT_SIZE + payload_len, GFP_KERNEL);
	if (!skb)
		goto dropped;

	hdr = genlmsg_put(skb, 0, 0, &melnode_genl_family, 0, MELNODE_CMD_PUNT);
	if (!hdr ||
	    nla_put_u32(skb, MELNODE_A_PKT_LINK, ingress_link) ||
	    nla_put_u8(skb, MELNODE_A_PKT_PROTO, proto) ||
	    nla_put_u8(skb, MELNODE_A_PKT_SRC, src) ||
	    nla_put_u8(skb, MELNODE_A_PKT_DST, dst) ||
	    nla_put_u8(skb, MELNODE_A_PKT_TTL, ttl) ||
	    nla_put(skb, MELNODE_A_PKT_DATA, payload_len, payload)) {
		nlmsg_free(skb);
		goto dropped;
	}
	genlmsg_end(skb, hdr);

	if (genlmsg_unicast(&init_net, skb, portid))
		goto dropped;

	melnode_stat_inc(MELNODE_STAT_PUNT_SENT);
	return;

dropped:
	melnode_stat_inc(MELNODE_STAT_PUNT_DROPPED);
}

static void melnode_send_event_handshake(u8 peer_id, const void *endpoint, int endpoint_len)
{
	struct sk_buff *skb;
	void *hdr;

	skb = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_KERNEL);
	if (!skb)
		goto dropped;

	hdr = genlmsg_put(skb, 0, 0, &melnode_genl_family, 0, MELNODE_CMD_EVENT);
	if (!hdr ||
	    nla_put_u8(skb, MELNODE_A_EVT_KIND, MELNODE_EVENT_LINK_HANDSHAKE) ||
	    nla_put_u32(skb, MELNODE_A_PEER_ID, peer_id) ||
	    nla_put_u64_64bit(skb, MELNODE_A_EVT_TIME, ktime_get_real_ns(), MELNODE_A_PAD) ||
	    nla_put(skb, MELNODE_A_ENDPOINT, endpoint_len, endpoint)) {
		nlmsg_free(skb);
		goto dropped;
	}
	genlmsg_end(skb, hdr);

	if (genlmsg_multicast(&melnode_genl_family, skb, 0, 0, GFP_KERNEL))
		goto dropped;
	return;

dropped:
	melnode_stat_inc(MELNODE_STAT_EVENTS_DROPPED);
}

static int melnode_reply_public_key(struct genl_info *info, const u8 public_key[32])
{
	struct sk_buff *reply;
	void *hdr;

	reply = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_KERNEL);
	if (!reply)
		return -ENOMEM;

	hdr = genlmsg_put_reply(reply, info, &melnode_genl_family, 0, MELNODE_CMD_DEVICE_SET);
	if (!hdr || nla_put(reply, MELNODE_A_PUBLIC_KEY, 32, public_key)) {
		nlmsg_free(reply);
		return -EMSGSIZE;
	}
	genlmsg_end(reply, hdr);
	return genlmsg_reply(reply, info);
}

static int melnode_nl_device_set(struct sk_buff *skb, struct genl_info *info)
{
	u8 private_key[32];
	u8 public_key[32];
	u32 local_id, mtu, fwmark = 0;
	u16 listen_port;
	bool identical;
	int err;

	if (!info->attrs[MELNODE_A_LOCAL_ID] || !info->attrs[MELNODE_A_PRIVATE_KEY] ||
	    !info->attrs[MELNODE_A_LISTEN_PORT] || !info->attrs[MELNODE_A_MTU])
		return -EINVAL;

	local_id = nla_get_u32(info->attrs[MELNODE_A_LOCAL_ID]);
	if (local_id > 255)
		return -EINVAL;
	listen_port = nla_get_u16(info->attrs[MELNODE_A_LISTEN_PORT]);
	mtu = nla_get_u32(info->attrs[MELNODE_A_MTU]);
	if (mtu < MELNODE_MIN_MTU || mtu > MELNODE_MAX_MTU)
		return -EINVAL;
	if (info->attrs[MELNODE_A_FWMARK])
		fwmark = nla_get_u32(info->attrs[MELNODE_A_FWMARK]);
	memcpy(private_key, nla_data(info->attrs[MELNODE_A_PRIVATE_KEY]), 32);

	if (!curve25519_generate_public(public_key, private_key)) {
		memzero_explicit(private_key, sizeof(private_key));
		return -EINVAL;
	}

	mutex_lock(&melnode_dev.config_lock);

	if (melnode_dev.configured) {
		identical = melnode_dev.local_id == local_id &&
			    melnode_dev.listen_port == listen_port && melnode_dev.mtu == mtu &&
			    melnode_dev.fwmark == fwmark &&
			    !memcmp(melnode_dev.private_key, private_key, 32);
		mutex_unlock(&melnode_dev.config_lock);
		memzero_explicit(private_key, sizeof(private_key));
		return identical ? melnode_reply_public_key(info, public_key) : -EEXIST;
	}

	err = melnode_socket_open(&melnode_dev, listen_port, fwmark);
	if (err) {
		mutex_unlock(&melnode_dev.config_lock);
		memzero_explicit(private_key, sizeof(private_key));
		return err;
	}

	WRITE_ONCE(melnode_dev.local_id, local_id);
	melnode_dev.listen_port = listen_port;
	melnode_dev.mtu = mtu;
	melnode_dev.fwmark = fwmark;

	spin_lock_bh(&melnode_dev.key_lock);
	memcpy(melnode_dev.private_key, private_key, 32);
	memcpy(melnode_dev.public_key, public_key, 32);
	spin_unlock_bh(&melnode_dev.key_lock);

	melnode_cookie_checker_init(&melnode_dev.cookie_checker);
	melnode_cookie_checker_precompute_device_keys(&melnode_dev.cookie_checker, public_key);
	WRITE_ONCE(melnode_dev.configured, true);
	mutex_unlock(&melnode_dev.config_lock);
	memzero_explicit(private_key, sizeof(private_key));

	return melnode_reply_public_key(info, public_key);
}

static void melnode_teardown_device(void)
{
	struct melnode_link *link, *link_tmp;
	LIST_HEAD(dead_tuns);
	LIST_HEAD(dead_links);
	int i;

	mutex_lock(&melnode_dev.config_lock);

	WRITE_ONCE(melnode_dev.configured, false);

	mutex_lock(&melnode_dev.tables_mutex);
	for (i = 0; i < MELNODE_MAX_PEER; i++) {
		struct melnode_route *route = rcu_dereference_protected(
			melnode_dev.routes[i], lockdep_is_held(&melnode_dev.tables_mutex));
		struct melnode_tun *tun = rcu_dereference_protected(
			melnode_dev.tuns[i], lockdep_is_held(&melnode_dev.tables_mutex));

		link = rcu_dereference_protected(melnode_dev.links[i],
						 lockdep_is_held(&melnode_dev.tables_mutex));

		if (route) {
			RCU_INIT_POINTER(melnode_dev.routes[i], NULL);
			kfree_rcu(route, rcu);
		}
		if (link) {
			RCU_INIT_POINTER(melnode_dev.links[i], NULL);
			list_add_tail(&link->teardown_node, &dead_links);
		}
		if (tun) {
			RCU_INIT_POINTER(melnode_dev.tuns[i], NULL);
			WRITE_ONCE(tun->state, MELNODE_TUN_DESTROYING);
			unregister_netdevice_queue(tun->dev, &dead_tuns);
			kfree_rcu(tun, rcu);
		}
	}
	mutex_unlock(&melnode_dev.tables_mutex);

	list_for_each_entry_safe(link, link_tmp, &dead_links, teardown_node) {
		flush_work(&link->outq_work);
		list_del(&link->teardown_node);
		melnode_link_put(link);
	}

	rtnl_lock();
	unregister_netdevice_many(&dead_tuns);
	rtnl_unlock();

	flush_work(&melnode_dev.tun_teardown_work);

	spin_lock_bh(&melnode_dev.key_lock);
	memzero_explicit(melnode_dev.private_key, sizeof(melnode_dev.private_key));
	memset(melnode_dev.public_key, 0, sizeof(melnode_dev.public_key));
	spin_unlock_bh(&melnode_dev.key_lock);

	melnode_socket_close(&melnode_dev);

	mutex_unlock(&melnode_dev.config_lock);
}

static int melnode_nl_device_del(struct sk_buff *skb, struct genl_info *info)
{
	melnode_teardown_device();
	return 0;
}

static int melnode_nl_stats_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	int id = cb->args[0] ?: 1;

	for (; id <= MELNODE_STAT_RX_QUEUE_FULL; id++) {
		s64 value = atomic64_read(&melnode_dev.stats[id]);
		void *hdr;

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				  &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_STATS_GET);
		if (!hdr)
			break;
		if (nla_put_u16(skb, MELNODE_A_STAT_ID, id) ||
		    nla_put_u64_64bit(skb, MELNODE_A_STAT_VALUE, value, MELNODE_A_PAD)) {
			genlmsg_cancel(skb, hdr);
			break;
		}
		genlmsg_end(skb, hdr);
	}

	cb->args[0] = id;
	return skb->len;
}

static int melnode_nl_link_add(struct sk_buff *skb, struct genl_info *info)
{
	struct nlattr *endpoint_attr = info->attrs[MELNODE_A_ENDPOINT];
	struct melnode_link *link;
	const u8 *pubkey;
	int endpoint_len = 0;
	u8 local_priv[32];
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;
	if (!info->attrs[MELNODE_A_PUBLIC_KEY])
		return -EINVAL;
	pubkey = nla_data(info->attrs[MELNODE_A_PUBLIC_KEY]);

	if (endpoint_attr) {
		endpoint_len = melnode_endpoint_len(nla_data(endpoint_attr), nla_len(endpoint_attr));
		if (endpoint_len < 0)
			return endpoint_len;
	}

	link = melnode_link_get(peer_id);
	if (link) {
		if (memcmp(link->public_key, pubkey, 32)) {
			melnode_link_put(link);
			return -EEXIST;
		}
		if (endpoint_attr) {
			spin_lock_bh(&link->lock);
			melnode_link_set_endpoint(link, nla_data(endpoint_attr), endpoint_len);
			spin_unlock_bh(&link->lock);
		}
		melnode_link_put(link);
		return 0;
	}

	link = kzalloc(sizeof(*link), GFP_KERNEL);
	if (!link)
		return -ENOMEM;
	err = melnode_routing_link_init(link);
	if (err) {
		kfree(link);
		return err;
	}
	link->peer_id = peer_id;
	memcpy(link->public_key, pubkey, 32);
	melnode_noise_handshake_init(&link->handshake, pubkey);
	melnode_get_keys(NULL, local_priv);
	melnode_noise_precompute_static_static(&link->handshake, local_priv, pubkey);
	memzero_explicit(local_priv, sizeof(local_priv));
	melnode_cookie_init(&link->cookie);
	melnode_cookie_precompute_peer_keys(&link->cookie, pubkey);

	if (endpoint_attr)
		melnode_link_set_endpoint(link, nla_data(endpoint_attr), endpoint_len);

	mutex_lock(&melnode_dev.tables_mutex);
	if (!melnode_configured()) {
		err = -ENODEV;
	} else if (rcu_dereference_protected(melnode_dev.links[peer_id],
					     lockdep_is_held(&melnode_dev.tables_mutex))) {
		err = -EEXIST;
	} else {
		rcu_assign_pointer(melnode_dev.links[peer_id], link);
		err = 0;
	}
	mutex_unlock(&melnode_dev.tables_mutex);

	if (err)
		melnode_link_put(link);
	return err;
}

static int melnode_nl_link_del(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_link *link;
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	mutex_lock(&melnode_dev.tables_mutex);
	link = rcu_dereference_protected(melnode_dev.links[peer_id],
					 lockdep_is_held(&melnode_dev.tables_mutex));
	RCU_INIT_POINTER(melnode_dev.links[peer_id], NULL);
	mutex_unlock(&melnode_dev.tables_mutex);

	if (!link)
		return -ENOENT;

	melnode_link_put(link);
	return 0;
}

static int melnode_nl_link_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	int peer_id = cb->args[0];

	for (; peer_id < MELNODE_MAX_PEER; peer_id++) {
		struct melnode_link *link;
		void *hdr;
		bool has_endpoint;
		u8 endpoint[sizeof(struct sockaddr_in6)];
		u8 endpoint_len = 0;
		u64 last_handshake;
		s64 tx, rx;

		rcu_read_lock();
		link = rcu_dereference(melnode_dev.links[peer_id]);
		if (!link) {
			rcu_read_unlock();
			continue;
		}
		spin_lock_bh(&link->lock);
		has_endpoint = link->has_endpoint;
		if (has_endpoint) {
			endpoint_len = link->endpoint_len;
			memcpy(endpoint, link->endpoint, endpoint_len);
		}
		last_handshake = link->last_handshake_unix_ns;
		spin_unlock_bh(&link->lock);
		tx = atomic64_read(&link->tx_bytes);
		rx = atomic64_read(&link->rx_bytes);

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				  &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_LINK_GET);
		if (!hdr) {
			rcu_read_unlock();
			break;
		}

		if (nla_put_u32(skb, MELNODE_A_PEER_ID, peer_id) ||
		    nla_put(skb, MELNODE_A_PUBLIC_KEY, 32, link->public_key) ||
		    (has_endpoint && nla_put(skb, MELNODE_A_ENDPOINT, endpoint_len, endpoint)) ||
		    nla_put_u64_64bit(skb, MELNODE_A_LAST_HANDSHAKE, last_handshake, MELNODE_A_PAD) ||
		    nla_put_u64_64bit(skb, MELNODE_A_TX_BYTES, tx, MELNODE_A_PAD) ||
		    nla_put_u64_64bit(skb, MELNODE_A_RX_BYTES, rx, MELNODE_A_PAD)) {
			genlmsg_cancel(skb, hdr);
			rcu_read_unlock();
			break;
		}
		genlmsg_end(skb, hdr);
		rcu_read_unlock();
	}

	cb->args[0] = peer_id;
	return skb->len;
}

static int melnode_nl_route_set(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_route *route, *old;
	u8 dst, nexthop;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_ROUTE_DST], &dst);
	if (err)
		return err;
	err = melnode_get_u8_id(info->attrs[MELNODE_A_ROUTE_NEXTHOP], &nexthop);
	if (err)
		return err;

	route = kmalloc(sizeof(*route), GFP_KERNEL);
	if (!route)
		return -ENOMEM;
	route->nexthop = nexthop;

	mutex_lock(&melnode_dev.tables_mutex);
	old = rcu_dereference_protected(melnode_dev.routes[dst],
					lockdep_is_held(&melnode_dev.tables_mutex));
	rcu_assign_pointer(melnode_dev.routes[dst], route);
	mutex_unlock(&melnode_dev.tables_mutex);

	if (old)
		kfree_rcu(old, rcu);
	return 0;
}

static int melnode_nl_route_del(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_route *old;
	u8 dst;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_ROUTE_DST], &dst);
	if (err)
		return err;

	mutex_lock(&melnode_dev.tables_mutex);
	old = rcu_dereference_protected(melnode_dev.routes[dst],
					lockdep_is_held(&melnode_dev.tables_mutex));
	RCU_INIT_POINTER(melnode_dev.routes[dst], NULL);
	mutex_unlock(&melnode_dev.tables_mutex);

	if (old)
		kfree_rcu(old, rcu);
	return 0;
}

static int melnode_nl_route_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	int dst = cb->args[0];

	for (; dst < MELNODE_MAX_PEER; dst++) {
		struct melnode_route *route;
		void *hdr;
		u8 nexthop;

		rcu_read_lock();
		route = rcu_dereference(melnode_dev.routes[dst]);
		if (!route) {
			rcu_read_unlock();
			continue;
		}
		nexthop = route->nexthop;
		rcu_read_unlock();

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				  &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_ROUTE_GET);
		if (!hdr)
			break;
		if (nla_put_u32(skb, MELNODE_A_ROUTE_DST, dst) ||
		    nla_put_u32(skb, MELNODE_A_ROUTE_NEXTHOP, nexthop)) {
			genlmsg_cancel(skb, hdr);
			break;
		}
		genlmsg_end(skb, hdr);
	}

	cb->args[0] = dst;
	return skb->len;
}

void melnode_send_initiation(struct melnode_link *link)
{
	struct melnode_wire_handshake_initiation msg;
	u8 endpoint[sizeof(struct sockaddr_in6)];
	u8 local_pub[32];
	u64 now = ktime_get_coarse_boottime_ns();
	int endpoint_len;
	bool created;

	melnode_get_keys(local_pub, NULL);

	spin_lock_bh(&link->lock);
	if (!link->has_endpoint ||
	    (s64)(link->handshake.last_sent_initiation + MELNODE_REKEY_TIMEOUT * NSEC_PER_SEC) >
		    (s64)now) {
		spin_unlock_bh(&link->lock);
		return;
	}
	link->handshake.last_sent_initiation = now;
	endpoint_len = link->endpoint_len;
	memcpy(endpoint, link->endpoint, endpoint_len);

	created = melnode_noise_handshake_create_initiation(&msg, &link->handshake, local_pub,
							    melnode_gen_index(link->peer_id));
	if (created)
		melnode_cookie_add_mac_to_packet(&msg, sizeof(msg), &link->cookie);
	spin_unlock_bh(&link->lock);

	if (created)
		melnode_socket_send(&melnode_dev, &msg, sizeof(msg), endpoint, endpoint_len);
}

static int melnode_tun_init(struct net_device *dev)
{
	struct melnode_tun_priv *priv = netdev_priv(dev);

	return gro_cells_init(&priv->gcells, dev);
}

static void melnode_tun_uninit(struct net_device *dev)
{
	struct melnode_tun_priv *priv = netdev_priv(dev);

	gro_cells_destroy(&priv->gcells);
}

static netdev_tx_t melnode_tun_xmit(struct sk_buff *skb, struct net_device *dev)
{
	struct melnode_tun_priv *priv = netdev_priv(dev);
	unsigned int len = skb->len;
	size_t plain_len = MELNODE_HDR_LEN + len;
	u8 *plain;
	int err;

	plain = kmalloc(plain_len, GFP_ATOMIC);
	if (!plain)
		goto drop;

	plain[MELNODE_HDR_OFF_PROTO] = 0;
	plain[MELNODE_HDR_OFF_SRC] = READ_ONCE(melnode_dev.local_id);
	plain[MELNODE_HDR_OFF_DST] = priv->peer_id;
	plain[MELNODE_HDR_OFF_TTL] = MELNODE_DEFAULT_TTL;
	if (skb_copy_bits(skb, 0, plain + MELNODE_HDR_LEN, len)) {
		kfree(plain);
		goto drop;
	}

	err = melnode_routing_route_and_send(priv->peer_id, plain, plain_len);
	if (err) {
		melnode_stat_inc(err == -ENOSPC ? MELNODE_STAT_TX_QUEUE_FULL :
						  MELNODE_STAT_TX_NO_ROUTE);
		goto drop;
	}

	consume_skb(skb);
	DEV_STATS_INC(dev, tx_packets);
	DEV_STATS_ADD(dev, tx_bytes, len);
	return NETDEV_TX_OK;

drop:
	kfree_skb(skb);
	DEV_STATS_INC(dev, tx_dropped);
	return NETDEV_TX_OK;
}

static const struct net_device_ops melnode_tun_netdev_ops = {
	.ndo_init = melnode_tun_init,
	.ndo_uninit = melnode_tun_uninit,
	.ndo_start_xmit = melnode_tun_xmit,
};

static void melnode_tun_setup(struct net_device *dev)
{
	dev->netdev_ops = &melnode_tun_netdev_ops;
	dev->type = ARPHRD_NONE;
	dev->flags = IFF_POINTOPOINT | IFF_NOARP | IFF_MULTICAST;
	dev->tx_queue_len = MELNODE_TUN_TX_QUEUE_LEN;
	dev->hard_header_len = 0;
	dev->addr_len = 0;
	dev->mtu = ETH_DATA_LEN;
	dev->min_mtu = MELNODE_MIN_MTU;
	dev->max_mtu = MELNODE_MAX_MTU;
	dev->needs_free_netdev = true;
}

static int melnode_reply_tun(struct genl_info *info, const char *name, bool started)
{
	struct sk_buff *reply;
	void *hdr;

	reply = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_KERNEL);
	if (!reply)
		return -ENOMEM;

	hdr = genlmsg_put_reply(reply, info, &melnode_genl_family, 0, MELNODE_CMD_TUN_CREATE);
	if (!hdr || nla_put_string(reply, MELNODE_A_TUN_NAME, name) ||
	    nla_put_u8(reply, MELNODE_A_TUN_STARTED, started)) {
		nlmsg_free(reply);
		return -EMSGSIZE;
	}
	genlmsg_end(reply, hdr);
	return genlmsg_reply(reply, info);
}

static int melnode_nl_tun_create(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_tun_priv *priv;
	struct net_device *dev;
	struct melnode_tun *tun;
	char name[IFNAMSIZ];
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;
	if (!info->attrs[MELNODE_A_TUN_NAME])
		return -EINVAL;
	nla_strscpy(name, info->attrs[MELNODE_A_TUN_NAME], sizeof(name));

	rcu_read_lock();
	tun = rcu_dereference(melnode_dev.tuns[peer_id]);
	if (tun) {
		enum melnode_tun_state state = READ_ONCE(tun->state);
		bool same_name;

		dev = tun->dev;
		dev_hold(dev);
		rcu_read_unlock();
		same_name = !strcmp(dev->name, name);
		dev_put(dev);
		if (!same_name)
			return -EEXIST;
		return melnode_reply_tun(info, name, state == MELNODE_TUN_STARTED);
	}
	rcu_read_unlock();

	rtnl_lock();
	dev = alloc_netdev(sizeof(*priv), name, NET_NAME_USER, melnode_tun_setup);
	if (!dev) {
		rtnl_unlock();
		return -ENOMEM;
	}
	dev_net_set(dev, &init_net);
	dev->mtu = READ_ONCE(melnode_dev.mtu);
	priv = netdev_priv(dev);
	priv->peer_id = peer_id;

	err = register_netdevice(dev);
	if (err) {
		free_netdev(dev);
		rtnl_unlock();
		return err;
	}
	rtnl_unlock();

	tun = kmalloc(sizeof(*tun), GFP_KERNEL);
	if (!tun) {
		err = -ENOMEM;
		goto err_unregister;
	}
	tun->peer_id = peer_id;
	tun->dev = dev;
	tun->state = MELNODE_TUN_CREATED;

	mutex_lock(&melnode_dev.tables_mutex);
	if (!melnode_configured()) {
		err = -ENODEV;
	} else if (rcu_dereference_protected(melnode_dev.tuns[peer_id],
					     lockdep_is_held(&melnode_dev.tables_mutex))) {
		err = -EEXIST;
	} else {
		rcu_assign_pointer(melnode_dev.tuns[peer_id], tun);
		err = 0;
	}
	mutex_unlock(&melnode_dev.tables_mutex);

	if (err) {
		kfree(tun);
		goto err_unregister;
	}

	return melnode_reply_tun(info, name, false);

err_unregister:
	rtnl_lock();
	unregister_netdevice(dev);
	rtnl_unlock();
	return err;
}

static int melnode_nl_tun_start(struct sk_buff *skb, struct genl_info *info)
{
	struct net_device *dev = NULL;
	struct melnode_tun *tun;
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	rcu_read_lock();
	tun = rcu_dereference(melnode_dev.tuns[peer_id]);
	if (!tun) {
		rcu_read_unlock();
		return -ENOENT;
	}
	if (READ_ONCE(tun->state) == MELNODE_TUN_CREATED) {
		dev = tun->dev;
		dev_hold(dev);
	}
	rcu_read_unlock();

	if (!dev)
		return 0;

	rtnl_lock();
	err = dev_open(dev, NULL);
	rtnl_unlock();
	dev_put(dev);
	if (err)
		return err;

	rcu_read_lock();
	if (rcu_dereference(melnode_dev.tuns[peer_id]) == tun)
		WRITE_ONCE(tun->state, MELNODE_TUN_STARTED);
	rcu_read_unlock();
	return 0;
}

static int melnode_nl_tun_destroy(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_tun *tun;
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	mutex_lock(&melnode_dev.tables_mutex);
	tun = rcu_dereference_protected(melnode_dev.tuns[peer_id],
					lockdep_is_held(&melnode_dev.tables_mutex));
	RCU_INIT_POINTER(melnode_dev.tuns[peer_id], NULL);
	mutex_unlock(&melnode_dev.tables_mutex);

	if (tun) {
		WRITE_ONCE(tun->state, MELNODE_TUN_DESTROYING);
		melnode_queue_tun_teardown(tun->dev);
		kfree_rcu(tun, rcu);
	}
	return 0;
}

static int melnode_nl_tun_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	int peer_id = cb->args[0];

	for (; peer_id < MELNODE_MAX_PEER; peer_id++) {
		struct melnode_tun *tun;
		struct net_device *dev;
		enum melnode_tun_state state;
		char name[IFNAMSIZ];
		u32 mtu;
		void *hdr;

		rcu_read_lock();
		tun = rcu_dereference(melnode_dev.tuns[peer_id]);
		if (!tun) {
			rcu_read_unlock();
			continue;
		}
		dev = tun->dev;
		dev_hold(dev);
		state = READ_ONCE(tun->state);
		rcu_read_unlock();

		strscpy(name, dev->name, sizeof(name));
		mtu = dev->mtu;
		dev_put(dev);

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				  &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_TUN_GET);
		if (!hdr)
			break;
		if (nla_put_u32(skb, MELNODE_A_PEER_ID, peer_id) ||
		    nla_put_string(skb, MELNODE_A_TUN_NAME, name) ||
		    nla_put_u32(skb, MELNODE_A_MTU, mtu) ||
		    nla_put_u8(skb, MELNODE_A_TUN_STARTED, state == MELNODE_TUN_STARTED)) {
			genlmsg_cancel(skb, hdr);
			break;
		}
		genlmsg_end(skb, hdr);
	}

	cb->args[0] = peer_id;
	return skb->len;
}

static int melnode_nl_inject(struct sk_buff *skb, struct genl_info *info)
{
	struct nlattr *data_attr = info->attrs[MELNODE_A_PKT_DATA];
	struct melnode_link *link;
	size_t payload_len = data_attr ? nla_len(data_attr) : 0;
	size_t plain_len = MELNODE_HDR_LEN + payload_len;
	u8 link_peer_id, ttl = MELNODE_DEFAULT_TTL;
	u8 *plain;
	int err;

	if (melnode_get_u8_id(info->attrs[MELNODE_A_PKT_LINK], &link_peer_id) ||
	    !info->attrs[MELNODE_A_PKT_PROTO] || !info->attrs[MELNODE_A_PKT_DST] ||
	    payload_len > READ_ONCE(melnode_dev.mtu))
		goto dropped;
	if (info->attrs[MELNODE_A_PKT_TTL])
		ttl = nla_get_u8(info->attrs[MELNODE_A_PKT_TTL]);

	link = melnode_link_get(link_peer_id);
	if (!link)
		goto dropped;

	plain = kmalloc(plain_len, GFP_KERNEL);
	if (!plain) {
		melnode_link_put(link);
		goto dropped;
	}
	plain[MELNODE_HDR_OFF_PROTO] = nla_get_u8(info->attrs[MELNODE_A_PKT_PROTO]);
	plain[MELNODE_HDR_OFF_SRC] = READ_ONCE(melnode_dev.local_id);
	plain[MELNODE_HDR_OFF_DST] = nla_get_u8(info->attrs[MELNODE_A_PKT_DST]);
	plain[MELNODE_HDR_OFF_TTL] = ttl;
	if (payload_len)
		memcpy(plain + MELNODE_HDR_LEN, nla_data(data_attr), payload_len);

	err = melnode_routing_send_now(link, plain, plain_len);
	kfree(plain);
	if (err == -ENOENT)
		melnode_send_initiation(link);
	melnode_link_put(link);
	if (err)
		goto dropped;

	melnode_stat_inc(MELNODE_STAT_INJECT_SENT);
	return 0;

dropped:
	melnode_stat_inc(MELNODE_STAT_INJECT_DROPPED);
	return 0;
}

static void melnode_handle_handshake_initiation(u8 *data, size_t len, const void *from,
						int from_len)
{
	struct melnode_wire_handshake_initiation *msg = (void *)data;
	struct melnode_handshake_precursor pre;
	struct melnode_wire_handshake_response resp;
	enum melnode_cookie_mac_state mac_state;
	struct melnode_link *link = NULL;
	u8 remote_static[32];
	u8 local_pub[32], local_priv[32];
	bool ok;
	int i;

	if (len != sizeof(*msg))
		return;

	mac_state = melnode_cookie_validate_packet(&melnode_dev.cookie_checker, data, len, from,
						   from_len, true);
	switch (mac_state) {
	case MELNODE_COOKIE_INVALID_MAC:
	case MELNODE_COOKIE_VALID_MAC_WITH_COOKIE_BUT_RATELIMITED:
		return;
	case MELNODE_COOKIE_VALID_MAC_BUT_NO_COOKIE: {
		struct melnode_wire_handshake_cookie reply;

		melnode_cookie_message_create(&reply, data, len, from, from_len, msg->sender_index,
					      &melnode_dev.cookie_checker);
		melnode_socket_send(&melnode_dev, &reply, sizeof(reply), from, from_len);
		return;
	}
	case MELNODE_COOKIE_VALID_MAC_WITH_COOKIE:
		break;
	}

	melnode_get_keys(local_pub, local_priv);
	ok = melnode_noise_handshake_consume_initiation1(msg, local_pub, local_priv, remote_static,
							 &pre);
	memzero_explicit(local_priv, sizeof(local_priv));
	if (!ok)
		goto out;

	rcu_read_lock();
	for (i = 0; i < MELNODE_MAX_PEER; i++) {
		struct melnode_link *cand = rcu_dereference(melnode_dev.links[i]);

		if (cand && !memcmp(cand->public_key, remote_static, 32)) {
			if (kref_get_unless_zero(&cand->kref))
				link = cand;
			break;
		}
	}
	rcu_read_unlock();
	if (!link)
		goto out;

	spin_lock_bh(&link->lock);
	ok = melnode_noise_handshake_consume_initiation2(msg, &pre, &link->handshake) &&
	     melnode_noise_handshake_create_response(&resp, &link->handshake,
						     melnode_gen_index(link->peer_id));
	if (ok) {
		melnode_link_set_endpoint(link, from, from_len);
		melnode_cookie_add_mac_to_packet(&resp, sizeof(resp), &link->cookie);
		melnode_noise_handshake_begin_session(&link->handshake, &link->keypairs);
		link->last_handshake_unix_ns = ktime_get_real_ns();
	}
	spin_unlock_bh(&link->lock);
	if (!ok)
		goto out;

	melnode_socket_send(&melnode_dev, &resp, sizeof(resp), from, from_len);
	melnode_send_event_handshake(link->peer_id, from, from_len);

out:
	if (link)
		melnode_link_put(link);
	memzero_explicit(&pre, sizeof(pre));
}

static void melnode_handle_handshake_response(u8 *data, size_t len, const void *from, int from_len)
{
	struct melnode_wire_handshake_response *msg = (void *)data;
	struct melnode_link *link;
	u8 local_priv[32];
	u8 peer_id;
	bool ok;

	if (len != sizeof(*msg))
		return;

	peer_id = melnode_index_peer_id(msg->receiver_index);
	link = melnode_link_get(peer_id);
	if (!link)
		return;

	if (melnode_cookie_validate_packet(&melnode_dev.cookie_checker, data, len, from, from_len,
					   false) == MELNODE_COOKIE_INVALID_MAC)
		goto out;

	melnode_get_keys(NULL, local_priv);

	spin_lock_bh(&link->lock);
	ok = melnode_noise_handshake_consume_response(msg, local_priv, &link->handshake);
	memzero_explicit(local_priv, sizeof(local_priv));
	if (ok) {
		melnode_link_set_endpoint(link, from, from_len);
		melnode_noise_handshake_begin_session(&link->handshake, &link->keypairs);
		link->last_handshake_unix_ns = ktime_get_real_ns();
	}
	spin_unlock_bh(&link->lock);
	if (!ok)
		goto out;

	melnode_routing_send_now(link, NULL, 0);
	melnode_send_event_handshake(peer_id, from, from_len);

out:
	melnode_link_put(link);
}

static void melnode_handle_cookie_reply(u8 *data, size_t len)
{
	struct melnode_wire_handshake_cookie *msg = (void *)data;
	struct melnode_link *link;

	if (len != sizeof(*msg))
		return;

	link = melnode_link_get(melnode_index_peer_id(msg->receiver_index));
	if (!link)
		return;

	spin_lock_bh(&link->lock);
	melnode_cookie_message_consume(msg, &link->cookie);
	spin_unlock_bh(&link->lock);
	melnode_link_put(link);
}

static struct melnode_keypair *melnode_find_keypair(struct melnode_link *link, __le32 index,
						    bool *is_next)
{
	struct melnode_keypairs *kps = &link->keypairs;

	*is_next = false;
	if (kps->current_kp.valid && kps->current_kp.local_index == index)
		return &kps->current_kp;
	if (kps->previous_kp.valid && kps->previous_kp.local_index == index)
		return &kps->previous_kp;
	if (kps->next_kp.valid && kps->next_kp.local_index == index) {
		*is_next = true;
		return &kps->next_kp;
	}
	return NULL;
}

static void melnode_handle_data(u8 *data, size_t len)
{
	struct melnode_wire_data *msg = (void *)data;
	u8 recv_key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	struct melnode_keypair *kp;
	struct melnode_link *link;
	size_t cipher_len, plain_len;
	bool is_next;
	u8 peer_id;
	u64 counter;
	u8 *plain;

	if (len < sizeof(*msg) + MELNODE_NOISE_AUTHTAG_LEN)
		return;
	cipher_len = len - sizeof(*msg);
	plain_len = cipher_len - MELNODE_NOISE_AUTHTAG_LEN;
	counter = le64_to_cpu(msg->counter);
	peer_id = melnode_index_peer_id(msg->receiver_index);

	link = melnode_link_get(peer_id);
	if (!link)
		return;

	spin_lock_bh(&link->lock);
	kp = melnode_find_keypair(link, msg->receiver_index, &is_next);
	if (!kp || !kp->receiving.is_valid) {
		spin_unlock_bh(&link->lock);
		goto bad_packet;
	}
	memcpy(recv_key, kp->receiving.key, sizeof(recv_key));
	spin_unlock_bh(&link->lock);

	plain = kmalloc(cipher_len, GFP_KERNEL);
	if (!plain) {
		memzero_explicit(recv_key, sizeof(recv_key));
		melnode_link_put(link);
		return;
	}
	if (!chacha20poly1305_decrypt(plain, msg->encrypted_data, cipher_len, NULL, 0, counter,
				      recv_key)) {
		memzero_explicit(recv_key, sizeof(recv_key));
		kfree(plain);
		goto bad_packet;
	}
	memzero_explicit(recv_key, sizeof(recv_key));

	spin_lock_bh(&link->lock);
	kp = melnode_find_keypair(link, msg->receiver_index, &is_next);
	if (!kp || !kp->receiving.is_valid ||
	    !melnode_replay_check(&kp->receiving_counter, counter)) {
		spin_unlock_bh(&link->lock);
		kfree(plain);
		goto bad_packet;
	}
	if (is_next)
		melnode_noise_received_with_keypair(&link->keypairs);
	atomic64_add(plain_len, &link->rx_bytes);
	spin_unlock_bh(&link->lock);
	melnode_link_put(link);

	if (!plain_len) {
		kfree(plain);
		return;
	}
	if (plain_len < MELNODE_HDR_LEN) {
		kfree(plain);
		melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
		return;
	}

	if (plain[MELNODE_HDR_OFF_PROTO] != 0) {
		melnode_send_punt(peer_id, plain[MELNODE_HDR_OFF_PROTO], plain[MELNODE_HDR_OFF_SRC],
				  plain[MELNODE_HDR_OFF_DST], plain[MELNODE_HDR_OFF_TTL],
				  plain + MELNODE_HDR_LEN, plain_len - MELNODE_HDR_LEN);
		kfree(plain);
		return;
	}

	melnode_routing_deliver_or_forward(plain[MELNODE_HDR_OFF_SRC], plain[MELNODE_HDR_OFF_DST],
					   plain[MELNODE_HDR_OFF_TTL], plain, plain_len);
	return;

bad_packet:
	melnode_link_put(link);
	melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
}

void melnode_handle_datagram(u8 *data, size_t len, const void *from_addr, int from_len)
{
	__le32 type;

	if (!melnode_configured() || len < sizeof(struct melnode_wire_header) ||
	    melnode_endpoint_len(from_addr, from_len) != from_len)
		return;
	memcpy(&type, data, sizeof(type));

	switch (le32_to_cpu(type)) {
	case MELNODE_WIRE_MSG_HANDSHAKE_INITIATION:
		melnode_handle_handshake_initiation(data, len, from_addr, from_len);
		break;
	case MELNODE_WIRE_MSG_HANDSHAKE_RESPONSE:
		melnode_handle_handshake_response(data, len, from_addr, from_len);
		break;
	case MELNODE_WIRE_MSG_HANDSHAKE_COOKIE:
		melnode_handle_cookie_reply(data, len);
		break;
	case MELNODE_WIRE_MSG_DATA:
		melnode_handle_data(data, len);
		break;
	}
}

static int melnode_pre_doit(const struct genl_split_ops *ops, struct sk_buff *skb,
			    struct genl_info *info)
{
	switch (ops->cmd) {
	case MELNODE_CMD_HELLO:
	case MELNODE_CMD_ATTACH:
	case MELNODE_CMD_DEVICE_SET:
	case MELNODE_CMD_DEVICE_DEL:
	case MELNODE_CMD_STATS_GET:
		return 0;
	default:
		return melnode_configured() ? 0 : -ENODEV;
	}
}

static const struct genl_ops melnode_ops[] = {
	{ .cmd = MELNODE_CMD_HELLO, .doit = melnode_nl_hello, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_ATTACH, .doit = melnode_nl_attach, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_DEVICE_SET, .doit = melnode_nl_device_set, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_DEVICE_DEL, .doit = melnode_nl_device_del, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_STATS_GET, .dumpit = melnode_nl_stats_get, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_LINK_ADD, .doit = melnode_nl_link_add, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_LINK_DEL, .doit = melnode_nl_link_del, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_LINK_GET, .dumpit = melnode_nl_link_get, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_ROUTE_SET, .doit = melnode_nl_route_set, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_ROUTE_DEL, .doit = melnode_nl_route_del, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_ROUTE_GET, .dumpit = melnode_nl_route_get, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_INJECT, .doit = melnode_nl_inject, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_TUN_CREATE, .doit = melnode_nl_tun_create, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_TUN_START, .doit = melnode_nl_tun_start, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_TUN_DESTROY, .doit = melnode_nl_tun_destroy, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_TUN_GET, .dumpit = melnode_nl_tun_get, .flags = GENL_ADMIN_PERM },
};

static struct genl_family melnode_genl_family __ro_after_init = {
	.name = MELNODE_GENL_NAME,
	.version = MELNODE_GENL_VERSION,
	.maxattr = MELNODE_A_MAX,
	.policy = melnode_policy,
	.module = THIS_MODULE,
	.pre_doit = melnode_pre_doit,
	.ops = melnode_ops,
	.n_ops = ARRAY_SIZE(melnode_ops),
	.mcgrps = melnode_mcgrps,
	.n_mcgrps = ARRAY_SIZE(melnode_mcgrps),
	.parallel_ops = true,
};

static int __init melnode_init(void)
{
	int err;

	melnode_noise_init();
	melnode_socket_init_work(&melnode_dev);
	INIT_WORK(&melnode_dev.tun_teardown_work, melnode_tun_teardown_work_fn);

	err = melnode_ratelimiter_init();
	if (err)
		return err;

	err = genl_register_family(&melnode_genl_family);
	if (err)
		goto err_ratelimiter;

	err = netlink_register_notifier(&melnode_netlink_notifier);
	if (err)
		goto err_family;

	pr_info("melnode: family \"%s\" v%d registered\n", MELNODE_GENL_NAME, MELNODE_GENL_VERSION);
	return 0;

err_family:
	genl_unregister_family(&melnode_genl_family);
err_ratelimiter:
	melnode_ratelimiter_uninit();
	return err;
}

static void __exit melnode_exit(void)
{
	netlink_unregister_notifier(&melnode_netlink_notifier);
	genl_unregister_family(&melnode_genl_family);
	melnode_teardown_device();
	melnode_ratelimiter_uninit();
	pr_info("melnode: unloaded\n");
}

module_init(melnode_init);
module_exit(melnode_exit);

MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("melnode kernel data plane (genl family \"melnode\")");
MODULE_SOFTDEP("pre: libcurve25519 libchacha20poly1305");
