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
#include <net/net_namespace.h>
#include <net/rtnetlink.h>
#include <crypto/curve25519.h>
#include <crypto/chacha20poly1305.h>

#include "melnode_compat.h"
#include "melnode_core.h"
#include "melnode_index.h"
#include "melnode_routing.h"
#include "melnode_socket.h"
#include "melnode_transport.h"
#include "melnode_ratelimiter.h"

#define MELNODE_A_MAX MELNODE_A_PKT_DATA
#define MELNODE_TUN_TX_QUEUE_LEN 500
#define MELNODE_LINK_KIND "melinoe-vpn"
#define SECS_TO_NS(s) ((u64)(s) * NSEC_PER_SEC)

static LIST_HEAD(melnode_instances);
static DEFINE_MUTEX(melnode_instances_lock);

struct workqueue_struct *melnode_wq;
static struct workqueue_struct *melnode_destroy_wq;

struct melnode_pending_teardown {
	struct list_head list;
	struct net_device *dev;
};

static struct genl_family melnode_genl_family;
static struct rtnl_link_ops melnode_link_ops;

static const struct nla_policy melnode_route_policy[MELNODE_A_ROUTE_NEXTHOP + 1] = {
	[MELNODE_A_ROUTE_DST] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_ROUTE_NEXTHOP] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
};

static const struct nla_policy melnode_policy[MELNODE_A_MAX + 1] = {
	[MELNODE_A_LOCAL_ID] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PRIVATE_KEY] = NLA_POLICY_EXACT_LEN(32),
	[MELNODE_A_LISTEN_PORT] = NLA_POLICY_EXACT_LEN(sizeof(u16)),
	[MELNODE_A_FWMARK] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_MTU] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PEER_ID] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PUBLIC_KEY] = NLA_POLICY_EXACT_LEN(32),
	[MELNODE_A_ENDPOINT] = NLA_POLICY_RANGE(NLA_BINARY, sizeof(struct sockaddr_in),
						sizeof(struct sockaddr_in6)),
	[MELNODE_A_ROUTES] = NLA_POLICY_NESTED_ARRAY(melnode_route_policy),
	[MELNODE_A_PKT_LINK] = NLA_POLICY_EXACT_LEN(sizeof(u32)),
	[MELNODE_A_PKT_PROTO] = NLA_POLICY_EXACT_LEN(sizeof(u8)),
	[MELNODE_A_PKT_DST] = NLA_POLICY_EXACT_LEN(sizeof(u8)),
	[MELNODE_A_PKT_TTL] = NLA_POLICY_EXACT_LEN(sizeof(u8)),
	[MELNODE_A_PKT_DATA] = { .type = NLA_BINARY },
	[MELNODE_A_TUN_NAME] = { .type = NLA_NUL_STRING, .len = IFNAMSIZ - 1 },
};

static void melnode_hs_drain(struct melnode_instance *inst);
static void melnode_hs_work_fn(struct work_struct *work);
static void melnode_instance_destroy_work_fn(struct work_struct *work);

static void melnode_tun_teardown_work_fn(struct work_struct *work)
{
	struct melnode_instance *inst =
		container_of(work, struct melnode_instance, tun_teardown_work);
	struct melnode_pending_teardown *p, *tmp;
	LIST_HEAD(local);
	LIST_HEAD(dead);

	spin_lock_bh(&inst->pending_teardown_lock);
	list_splice_init(&inst->pending_teardown, &local);
	spin_unlock_bh(&inst->pending_teardown_lock);

	if (list_empty(&local))
		return;

	rtnl_lock();
	list_for_each_entry(p, &local, list) {
		if (p->dev->reg_state == NETREG_REGISTERED)
			unregister_netdevice_queue(p->dev, &dead);
	}
	unregister_netdevice_many(&dead);
	list_for_each_entry(p, &local, list)
		dev_put(p->dev);
	rtnl_unlock();

	list_for_each_entry_safe(p, tmp, &local, list) {
		list_del(&p->list);
		kfree(p);
	}
}

static void melnode_queue_tun_teardown(struct melnode_instance *inst, struct net_device *dev)
{
	struct melnode_pending_teardown *p;

	p = kmalloc(sizeof(*p), GFP_KERNEL);
	if (!p) {
		rtnl_lock();
		if (dev->reg_state == NETREG_REGISTERED)
			unregister_netdevice(dev);
		dev_put(dev);
		rtnl_unlock();
		return;
	}
	p->dev = dev;
	spin_lock_bh(&inst->pending_teardown_lock);
	list_add_tail(&p->list, &inst->pending_teardown);
	spin_unlock_bh(&inst->pending_teardown_lock);
	queue_work(melnode_wq, &inst->tun_teardown_work);
}

void melnode_stat_inc(struct melnode_instance *inst, enum melnode_stat id)
{
	if (id > 0 && id <= MELNODE_STAT_RX_QUEUE_FULL)
		atomic64_inc(&inst->stats[id]);
}

void melnode_get_keys(struct melnode_instance *inst, u8 public_key[32], u8 private_key[32])
{
	spin_lock_bh(&inst->key_lock);
	if (public_key)
		memcpy(public_key, inst->public_key, 32);
	if (private_key)
		memcpy(private_key, inst->private_key, 32);
	spin_unlock_bh(&inst->key_lock);
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

static int melnode_endpoint_len(const void *data, int len)
{
	const struct sockaddr *sa = data;

	if (sa->sa_family == AF_INET && len >= sizeof(struct sockaddr_in))
		return sizeof(struct sockaddr_in);
	if (sa->sa_family == AF_INET6 && len >= sizeof(struct sockaddr_in6))
		return sizeof(struct sockaddr_in6);
	return -EINVAL;
}

static struct melnode_instance *melnode_instance_find_locked(struct net *net, u32 portid)
{
	struct melnode_instance *inst;

	list_for_each_entry(inst, &melnode_instances, list) {
		if (inst->portid == portid && net_eq(inst->net, net))
			return inst;
	}
	return NULL;
}

static struct melnode_instance *melnode_instance_get(struct net *net, u32 portid)
{
	struct melnode_instance *inst;

	mutex_lock(&melnode_instances_lock);
	inst = melnode_instance_find_locked(net, portid);
	if (inst)
		kref_get(&inst->kref);
	mutex_unlock(&melnode_instances_lock);
	return inst;
}

void melnode_instance_release(struct kref *kref)
{
	kfree(container_of(kref, struct melnode_instance, kref));
}

static int melnode_netlink_notifier_call(struct notifier_block *nb, unsigned long state,
					 void *_notify)
{
	struct netlink_notify *notify = _notify;
	struct melnode_instance *inst;

	if (state != NETLINK_URELEASE || notify->protocol != NETLINK_GENERIC)
		return NOTIFY_DONE;

	mutex_lock(&melnode_instances_lock);
	inst = melnode_instance_find_locked(notify->net, notify->portid);
	if (inst)
		list_del_init(&inst->list);
	mutex_unlock(&melnode_instances_lock);

	if (inst)
		queue_work(melnode_destroy_wq, &inst->destroy_work);
	return NOTIFY_DONE;
}

static struct notifier_block melnode_netlink_notifier = {
	.notifier_call = melnode_netlink_notifier_call,
};

static void melnode_send_punt(struct melnode_instance *inst, u8 ingress_link, u8 proto, u8 src,
			      u8 dst, u8 ttl, const u8 *payload, size_t payload_len)
{
	struct sk_buff *skb;
	void *hdr;

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

	if (genlmsg_unicast(inst->net, skb, inst->portid))
		goto dropped;

	melnode_stat_inc(inst, MELNODE_STAT_PUNT_SENT);
	return;

dropped:
	melnode_stat_inc(inst, MELNODE_STAT_PUNT_DROPPED);
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

static struct melnode_instance *melnode_instance_create(struct net *net, u32 portid, u32 local_id,
							u16 listen_port, u32 mtu, u32 fwmark,
							const u8 private_key[32],
							const u8 public_key[32])
{
	struct melnode_instance *inst;
	int err;

	inst = kzalloc(sizeof(*inst), GFP_KERNEL);
	if (!inst)
		return ERR_PTR(-ENOMEM);

	kref_init(&inst->kref);
	INIT_LIST_HEAD(&inst->list);
	INIT_WORK(&inst->destroy_work, melnode_instance_destroy_work_fn);
	inst->net = get_net(net);
	inst->portid = portid;
	inst->local_id = local_id;
	inst->listen_port = listen_port;
	inst->mtu = mtu;
	inst->fwmark = fwmark;

	spin_lock_init(&inst->key_lock);
	memcpy(inst->private_key, private_key, 32);
	memcpy(inst->public_key, public_key, 32);

	mutex_init(&inst->tables_mutex);
	init_rwsem(&inst->sock_sem);
	spin_lock_init(&inst->hs_lock);
	INIT_LIST_HEAD(&inst->hs_queue);
	INIT_WORK(&inst->hs_work, melnode_hs_work_fn);
	mutex_init(&inst->tun_create_lock);
	spin_lock_init(&inst->pending_teardown_lock);
	INIT_LIST_HEAD(&inst->pending_teardown);
	INIT_WORK(&inst->tun_teardown_work, melnode_tun_teardown_work_fn);
	melnode_socket_init_work(inst);
	melnode_index_table_init(&inst->index);

	err = melnode_ratelimiter_init(&inst->ratelimiter);
	if (err)
		goto err_free;
	melnode_cookie_checker_init(&inst->cookie_checker, &inst->ratelimiter);
	melnode_cookie_checker_precompute_device_keys(&inst->cookie_checker, public_key);

	err = melnode_socket_open(inst, listen_port, fwmark);
	if (err == -EADDRINUSE) {
		flush_workqueue(melnode_destroy_wq);
		err = melnode_socket_open(inst, listen_port, fwmark);
	}
	if (err)
		goto err_ratelimiter;
	return inst;

err_ratelimiter:
	melnode_ratelimiter_uninit(&inst->ratelimiter);
err_free:
	put_net(inst->net);
	memzero_explicit(inst->private_key, sizeof(inst->private_key));
	kfree(inst);
	return ERR_PTR(err);
}

static int melnode_nl_device_set(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_instance *inst;
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
	if (!listen_port)
		return -EINVAL;
	mtu = nla_get_u32(info->attrs[MELNODE_A_MTU]);
	if (mtu < MELNODE_MIN_MTU || mtu > MELNODE_MAX_MTU)
		return -EINVAL;
	if (info->attrs[MELNODE_A_FWMARK])
		fwmark = nla_get_u32(info->attrs[MELNODE_A_FWMARK]);
	memcpy(private_key, nla_data(info->attrs[MELNODE_A_PRIVATE_KEY]), 32);

	if (!memchr_inv(private_key, 0, sizeof(private_key)) ||
	    !curve25519_generate_public(public_key, private_key)) {
		memzero_explicit(private_key, sizeof(private_key));
		return -EINVAL;
	}

	mutex_lock(&melnode_instances_lock);

	inst = melnode_instance_find_locked(genl_info_net(info), info->snd_portid);
	if (inst) {
		identical = inst->local_id == local_id && inst->listen_port == listen_port &&
			    inst->mtu == mtu && inst->fwmark == fwmark &&
			    !memcmp(inst->private_key, private_key, 32);
		mutex_unlock(&melnode_instances_lock);
		memzero_explicit(private_key, sizeof(private_key));
		return identical ? melnode_reply_public_key(info, public_key) : -EEXIST;
	}

	inst = melnode_instance_create(genl_info_net(info), info->snd_portid, local_id,
				       listen_port, mtu, fwmark, private_key, public_key);
	memzero_explicit(private_key, sizeof(private_key));
	if (IS_ERR(inst)) {
		err = PTR_ERR(inst);
		mutex_unlock(&melnode_instances_lock);
		return err;
	}
	list_add_tail(&inst->list, &melnode_instances);
	mutex_unlock(&melnode_instances_lock);

	return melnode_reply_public_key(info, public_key);
}

static void melnode_instance_destroy(struct melnode_instance *inst)
{
	struct melnode_routes *routes;
	struct melnode_link *link, *link_tmp;
	LIST_HEAD(dead_tuns);
	LIST_HEAD(dead_links);
	int i;

	melnode_socket_close(inst);
	cancel_work_sync(&inst->hs_work);
	melnode_hs_drain(inst);

	rtnl_lock();
	mutex_lock(&inst->tables_mutex);
	inst->dead = true;
	routes = rcu_dereference_protected(inst->routes, lockdep_is_held(&inst->tables_mutex));
	RCU_INIT_POINTER(inst->routes, NULL);
	if (routes)
		kfree_rcu(routes, rcu);
	for (i = 0; i < MELNODE_MAX_PEER; i++) {
		struct net_device *dev = rcu_dereference_protected(
			inst->tuns[i], lockdep_is_held(&inst->tables_mutex));

		link = rcu_dereference_protected(inst->links[i],
						 lockdep_is_held(&inst->tables_mutex));
		if (link) {
			RCU_INIT_POINTER(inst->links[i], NULL);
			list_add_tail(&link->teardown_node, &dead_links);
		}
		if (dev) {
			RCU_INIT_POINTER(inst->tuns[i], NULL);
			unregister_netdevice_queue(dev, &dead_tuns);
		}
	}
	mutex_unlock(&inst->tables_mutex);
	unregister_netdevice_many(&dead_tuns);
	rtnl_unlock();

	list_for_each_entry_safe(link, link_tmp, &dead_links, teardown_node) {
		melnode_link_shutdown(link);
		flush_work(&link->outq_work);
		list_del(&link->teardown_node);
		melnode_link_put(link);
	}

	flush_work(&inst->tun_teardown_work);

	spin_lock_bh(&inst->key_lock);
	memzero_explicit(inst->private_key, sizeof(inst->private_key));
	memset(inst->public_key, 0, sizeof(inst->public_key));
	spin_unlock_bh(&inst->key_lock);

	melnode_ratelimiter_uninit(&inst->ratelimiter);
	put_net(inst->net);
	melnode_instance_put(inst);
}

static void melnode_instance_destroy_work_fn(struct work_struct *work)
{
	melnode_instance_destroy(container_of(work, struct melnode_instance, destroy_work));
}

static struct melnode_instance *melnode_dump_instance(struct netlink_callback *cb)
{
	return melnode_instance_get(sock_net(cb->skb->sk), NETLINK_CB(cb->skb).portid);
}

static int melnode_nl_stats_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	struct melnode_instance *inst = melnode_dump_instance(cb);
	int id = cb->args[0] ?: 1;

	if (!inst)
		return -ENODEV;

	for (; id <= MELNODE_STAT_RX_QUEUE_FULL; id++) {
		s64 value = atomic64_read(&inst->stats[id]);
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
	melnode_instance_put(inst);
	return skb->len;
}

static bool melnode_pubkey_in_use(struct melnode_instance *inst, const u8 *pubkey)
{
	int i;

	for (i = 0; i < MELNODE_MAX_PEER; i++) {
		struct melnode_link *link = rcu_dereference_protected(
			inst->links[i], lockdep_is_held(&inst->tables_mutex));

		if (link && !memcmp(link->public_key, pubkey, 32))
			return true;
	}
	return false;
}

static int melnode_nl_link_set(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_instance *inst = info->user_ptr[0];
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
	if (!info->attrs[MELNODE_A_PUBLIC_KEY] || peer_id == READ_ONCE(inst->local_id))
		return -EINVAL;
	pubkey = nla_data(info->attrs[MELNODE_A_PUBLIC_KEY]);

	if (endpoint_attr) {
		endpoint_len = melnode_endpoint_len(nla_data(endpoint_attr), nla_len(endpoint_attr));
		if (endpoint_len < 0)
			return endpoint_len;
	}

	link = melnode_link_get(inst, peer_id);
	if (link) {
		if (memcmp(link->public_key, pubkey, 32)) {
			melnode_link_put(link);
			return -EEXIST;
		}
		if (endpoint_attr) {
			spin_lock_bh(&link->lock);
			melnode_link_set_endpoint_configured_locked(link, nla_data(endpoint_attr),
								    endpoint_len);
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
	kref_get(&inst->kref);
	link->inst = inst;
	link->peer_id = peer_id;
	memcpy(link->public_key, pubkey, 32);
	melnode_noise_handshake_init(&link->handshake, pubkey);
	melnode_get_keys(inst, NULL, local_priv);
	melnode_noise_precompute_static_static(&link->handshake, local_priv, pubkey);
	memzero_explicit(local_priv, sizeof(local_priv));
	melnode_cookie_init(&link->cookie);
	melnode_cookie_precompute_peer_keys(&link->cookie, pubkey);

	if (endpoint_attr)
		melnode_link_set_endpoint_configured_locked(link, nla_data(endpoint_attr),
							    endpoint_len);

	mutex_lock(&inst->tables_mutex);
	if (inst->dead) {
		err = -ENODEV;
	} else if (rcu_dereference_protected(inst->links[peer_id],
					     lockdep_is_held(&inst->tables_mutex)) ||
		   melnode_pubkey_in_use(inst, pubkey)) {
		err = -EEXIST;
	} else {
		rcu_assign_pointer(inst->links[peer_id], link);
		err = 0;
	}
	mutex_unlock(&inst->tables_mutex);

	if (err)
		melnode_link_put(link);
	return err;
}

static int melnode_nl_link_del(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_instance *inst = info->user_ptr[0];
	struct melnode_link *link;
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	mutex_lock(&inst->tables_mutex);
	link = rcu_dereference_protected(inst->links[peer_id],
					 lockdep_is_held(&inst->tables_mutex));
	RCU_INIT_POINTER(inst->links[peer_id], NULL);
	mutex_unlock(&inst->tables_mutex);

	if (!link)
		return -ENOENT;

	melnode_link_shutdown(link);
	flush_work(&link->outq_work);
	melnode_link_put(link);
	return 0;
}

static int melnode_nl_link_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	struct melnode_instance *inst = melnode_dump_instance(cb);
	int peer_id = cb->args[0];

	if (!inst)
		return -ENODEV;

	for (; peer_id < MELNODE_MAX_PEER; peer_id++) {
		struct melnode_link *link;
		void *hdr;
		bool has_endpoint;
		u8 endpoint[sizeof(struct sockaddr_in6)];
		u8 endpoint_len = 0;
		u64 last_handshake;
		s64 tx, rx;

		rcu_read_lock();
		link = rcu_dereference(inst->links[peer_id]);
		if (!link) {
			rcu_read_unlock();
			continue;
		}
		spin_lock_bh(&link->lock);
		has_endpoint = link->has_endpoint;
		if (has_endpoint) {
			endpoint_len = link->endpoint.addr_len;
			memcpy(endpoint, link->endpoint.addr, endpoint_len);
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
	melnode_instance_put(inst);
	return skb->len;
}

static int melnode_nl_route_set(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_instance *inst = info->user_ptr[0];
	struct nlattr *tb[MELNODE_A_ROUTE_NEXTHOP + 1];
	struct melnode_routes *routes = NULL, *old;
	struct nlattr *entry;
	int rem, err;

	if (!info->attrs[MELNODE_A_ROUTES])
		return -EINVAL;

	nla_for_each_nested(entry, info->attrs[MELNODE_A_ROUTES], rem) {
		u8 dst, nexthop;

		if (nla_type(entry) != MELNODE_A_ROUTE) {
			err = -EINVAL;
			goto err_free;
		}
		err = nla_parse_nested(tb, MELNODE_A_ROUTE_NEXTHOP, entry, melnode_route_policy,
				       info->extack);
		if (err)
			goto err_free;
		err = melnode_get_u8_id(tb[MELNODE_A_ROUTE_DST], &dst);
		if (err)
			goto err_free;
		err = melnode_get_u8_id(tb[MELNODE_A_ROUTE_NEXTHOP], &nexthop);
		if (err)
			goto err_free;

		if (!routes) {
			routes = kzalloc(sizeof(*routes), GFP_KERNEL);
			if (!routes)
				return -ENOMEM;
		}
		if (test_and_set_bit(dst, routes->present)) {
			err = -EINVAL;
			goto err_free;
		}
		routes->nexthop[dst] = nexthop;
	}

	mutex_lock(&inst->tables_mutex);
	if (inst->dead) {
		mutex_unlock(&inst->tables_mutex);
		err = -ENODEV;
		goto err_free;
	}
	old = rcu_dereference_protected(inst->routes, lockdep_is_held(&inst->tables_mutex));
	rcu_assign_pointer(inst->routes, routes);
	mutex_unlock(&inst->tables_mutex);

	if (old)
		kfree_rcu(old, rcu);
	return 0;

err_free:
	kfree(routes);
	return err;
}

static int melnode_nl_route_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	struct melnode_instance *inst = melnode_dump_instance(cb);
	struct melnode_routes *routes;
	int dst = cb->args[0];

	if (!inst)
		return -ENODEV;

	rcu_read_lock();
	routes = rcu_dereference(inst->routes);
	for (; routes && dst < MELNODE_MAX_PEER; dst++) {
		void *hdr;

		if (!test_bit(dst, routes->present))
			continue;

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				  &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_ROUTE_GET);
		if (!hdr)
			break;
		if (nla_put_u32(skb, MELNODE_A_ROUTE_DST, dst) ||
		    nla_put_u32(skb, MELNODE_A_ROUTE_NEXTHOP, routes->nexthop[dst])) {
			genlmsg_cancel(skb, hdr);
			break;
		}
		genlmsg_end(skb, hdr);
	}
	rcu_read_unlock();

	cb->args[0] = dst;
	melnode_instance_put(inst);
	return skb->len;
}

void melnode_send_initiation(struct melnode_link *link, bool retry)
{
	struct melnode_instance *inst = link->inst;
	struct melnode_wire_handshake_initiation msg;
	struct melnode_endpoint endpoint;
	u8 local_pub[32];
	u64 now = ktime_get_coarse_boottime_ns();
	bool created = false;
	u32 index;

	melnode_get_keys(inst, local_pub, NULL);

	spin_lock_bh(&link->lock);
	if (!retry)
		link->handshake_attempts = 0;
	if ((s64)(link->handshake.last_sent_initiation + SECS_TO_NS(MELNODE_REKEY_TIMEOUT)) >
		    (s64)now ||
	    !melnode_link_endpoint_take_locked(link, &endpoint)) {
		spin_unlock_bh(&link->lock);
		return;
	}
	link->handshake.last_sent_initiation = now;

	index = melnode_index_alloc(link);
	if (index)
		created = melnode_noise_handshake_create_initiation(&msg, &link->handshake, local_pub,
								    index);
	if (created) {
		melnode_cookie_add_mac_to_packet(&msg, sizeof(msg), &link->cookie);
		link->keepalive_at = 0;
		link->retry_at = now + SECS_TO_NS(MELNODE_REKEY_TIMEOUT) + melnode_rekey_jitter_ns();
		melnode_link_arm_timer_locked(link);
	}
	melnode_index_sync(link);
	spin_unlock_bh(&link->lock);

	if (created && melnode_socket_send(inst, &msg, sizeof(msg), &endpoint,
				 MELNODE_HANDSHAKE_DSCP) >= 0)
		atomic64_add(sizeof(msg), &link->tx_bytes);
}

static int melnode_tun_init(struct net_device *dev)
{
	struct melnode_tun_priv *priv = netdev_priv(dev);
	int err;

	err = gro_cells_init(&priv->gcells, dev);
	if (err)
		return err;
	kref_get(&priv->inst->kref);
	return 0;
}

static void melnode_tun_uninit(struct net_device *dev)
{
	struct melnode_tun_priv *priv = netdev_priv(dev);

	gro_cells_destroy(&priv->gcells);
	melnode_instance_put(priv->inst);
}

static netdev_tx_t melnode_tun_xmit(struct sk_buff *skb, struct net_device *dev)
{
	struct melnode_tun_priv *priv = netdev_priv(dev);
	struct melnode_instance *inst = priv->inst;
	unsigned int len = skb->len;
	size_t plain_len = MELNODE_HDR_LEN + len;
	u8 *plain;
	int err;

	plain = kmalloc(plain_len, GFP_ATOMIC);
	if (!plain)
		goto drop;

	plain[MELNODE_HDR_OFF_PROTO] = 0;
	plain[MELNODE_HDR_OFF_SRC] = READ_ONCE(inst->local_id);
	plain[MELNODE_HDR_OFF_DST] = priv->peer_id;
	plain[MELNODE_HDR_OFF_TTL] = MELNODE_DEFAULT_TTL;
	if (skb_copy_bits(skb, 0, plain + MELNODE_HDR_LEN, len)) {
		kfree(plain);
		goto drop;
	}

	err = melnode_routing_route_and_send(inst, priv->peer_id, plain, plain_len);
	if (err) {
		melnode_stat_inc(inst, err == -ENOSPC ? MELNODE_STAT_TX_QUEUE_FULL :
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
	dev->rtnl_link_ops = &melnode_link_ops;
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

static void melnode_tun_dellink(struct net_device *dev, struct list_head *head)
{
	struct melnode_tun_priv *priv = netdev_priv(dev);
	struct melnode_instance *inst = priv->inst;

	mutex_lock(&inst->tables_mutex);
	if (rcu_access_pointer(inst->tuns[priv->peer_id]) == dev)
		RCU_INIT_POINTER(inst->tuns[priv->peer_id], NULL);
	mutex_unlock(&inst->tables_mutex);
	unregister_netdevice_queue(dev, head);
}

static struct rtnl_link_ops melnode_link_ops __read_mostly = {
	.kind = MELNODE_LINK_KIND,
	.priv_size = sizeof(struct melnode_tun_priv),
	.setup = melnode_tun_setup,
	.dellink = melnode_tun_dellink,
};

static int melnode_reply_tun(struct genl_info *info, u8 peer_id, const char *name, u32 ifindex)
{
	struct sk_buff *reply;
	void *hdr;

	reply = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_KERNEL);
	if (!reply)
		return -ENOMEM;

	hdr = genlmsg_put_reply(reply, info, &melnode_genl_family, 0, MELNODE_CMD_TUN_SET);
	if (!hdr || nla_put_u32(reply, MELNODE_A_PEER_ID, peer_id) ||
	    nla_put_string(reply, MELNODE_A_TUN_NAME, name) ||
	    nla_put_u32(reply, MELNODE_A_TUN_IFINDEX, ifindex)) {
		nlmsg_free(reply);
		return -EMSGSIZE;
	}
	genlmsg_end(reply, hdr);
	return genlmsg_reply(reply, info);
}

static int __melnode_tun_set(struct melnode_instance *inst, struct genl_info *info, u8 peer_id,
			     const char *name, bool *name_taken)
{
	struct melnode_tun_priv *priv;
	struct net_device *dev;
	char existing[IFNAMSIZ];
	u32 ifindex;
	int err;

	rcu_read_lock();
	dev = rcu_dereference(inst->tuns[peer_id]);
	if (dev) {
		strscpy(existing, dev->name, sizeof(existing));
		ifindex = dev->ifindex;
		rcu_read_unlock();
		if (strcmp(existing, name))
			return -EEXIST;
		return melnode_reply_tun(info, peer_id, existing, ifindex);
	}
	rcu_read_unlock();

	rtnl_lock();
	dev = alloc_netdev(sizeof(*priv), name, NET_NAME_USER, melnode_tun_setup);
	if (!dev) {
		rtnl_unlock();
		return -ENOMEM;
	}
	dev_net_set(dev, inst->net);
	dev->mtu = READ_ONCE(inst->mtu);
	priv = netdev_priv(dev);
	priv->peer_id = peer_id;
	priv->inst = inst;

	err = register_netdevice(dev);
	if (err) {
		*name_taken = err == -EEXIST;
		free_netdev(dev);
		rtnl_unlock();
		return err;
	}

	mutex_lock(&inst->tables_mutex);
	if (inst->dead) {
		err = -ENODEV;
	} else {
		rcu_assign_pointer(inst->tuns[peer_id], dev);
		err = 0;
	}
	mutex_unlock(&inst->tables_mutex);

	if (err) {
		unregister_netdevice(dev);
		rtnl_unlock();
		return err;
	}
	strscpy(existing, dev->name, sizeof(existing));
	ifindex = dev->ifindex;
	rtnl_unlock();

	return melnode_reply_tun(info, peer_id, existing, ifindex);
}

static int melnode_nl_tun_set(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_instance *inst = info->user_ptr[0];
	bool name_taken = false;
	char name[IFNAMSIZ];
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;
	if (!info->attrs[MELNODE_A_TUN_NAME] || peer_id == READ_ONCE(inst->local_id))
		return -EINVAL;
	melnode_compat_nla_strscpy(name, info->attrs[MELNODE_A_TUN_NAME], sizeof(name));

	mutex_lock(&inst->tun_create_lock);
	flush_work(&inst->tun_teardown_work);
	err = __melnode_tun_set(inst, info, peer_id, name, &name_taken);
	if (err && name_taken) {
		flush_workqueue(melnode_destroy_wq);
		err = __melnode_tun_set(inst, info, peer_id, name, &name_taken);
	}
	mutex_unlock(&inst->tun_create_lock);
	return err;
}

static int melnode_nl_tun_del(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_instance *inst = info->user_ptr[0];
	struct net_device *dev;
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	mutex_lock(&inst->tables_mutex);
	dev = rcu_dereference_protected(inst->tuns[peer_id], lockdep_is_held(&inst->tables_mutex));
	RCU_INIT_POINTER(inst->tuns[peer_id], NULL);
	if (dev)
		dev_hold(dev);
	mutex_unlock(&inst->tables_mutex);

	if (dev)
		melnode_queue_tun_teardown(inst, dev);
	return 0;
}

static int melnode_nl_tun_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	struct melnode_instance *inst = melnode_dump_instance(cb);
	int peer_id = cb->args[0];

	if (!inst)
		return -ENODEV;

	for (; peer_id < MELNODE_MAX_PEER; peer_id++) {
		struct net_device *dev;
		char name[IFNAMSIZ];
		u32 ifindex;
		void *hdr;

		rcu_read_lock();
		dev = rcu_dereference(inst->tuns[peer_id]);
		if (!dev) {
			rcu_read_unlock();
			continue;
		}
		strscpy(name, dev->name, sizeof(name));
		ifindex = dev->ifindex;
		rcu_read_unlock();

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				  &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_TUN_GET);
		if (!hdr)
			break;
		if (nla_put_u32(skb, MELNODE_A_PEER_ID, peer_id) ||
		    nla_put_string(skb, MELNODE_A_TUN_NAME, name) ||
		    nla_put_u32(skb, MELNODE_A_TUN_IFINDEX, ifindex)) {
			genlmsg_cancel(skb, hdr);
			break;
		}
		genlmsg_end(skb, hdr);
	}

	cb->args[0] = peer_id;
	melnode_instance_put(inst);
	return skb->len;
}

static int melnode_nl_inject(struct sk_buff *skb, struct genl_info *info)
{
	struct melnode_instance *inst = info->user_ptr[0];
	struct nlattr *data_attr = info->attrs[MELNODE_A_PKT_DATA];
	struct melnode_link *link;
	size_t payload_len = data_attr ? nla_len(data_attr) : 0;
	size_t plain_len = MELNODE_HDR_LEN + payload_len;
	u8 link_peer_id, ttl = MELNODE_DEFAULT_TTL;
	u8 *plain;
	int err;

	if (melnode_get_u8_id(info->attrs[MELNODE_A_PKT_LINK], &link_peer_id) ||
	    !info->attrs[MELNODE_A_PKT_PROTO] || !info->attrs[MELNODE_A_PKT_DST] ||
	    payload_len > READ_ONCE(inst->mtu))
		goto dropped;
	if (info->attrs[MELNODE_A_PKT_TTL])
		ttl = nla_get_u8(info->attrs[MELNODE_A_PKT_TTL]);

	link = melnode_link_get(inst, link_peer_id);
	if (!link)
		goto dropped;

	plain = kmalloc(plain_len, GFP_KERNEL);
	if (!plain) {
		melnode_link_put(link);
		goto dropped;
	}
	plain[MELNODE_HDR_OFF_PROTO] = nla_get_u8(info->attrs[MELNODE_A_PKT_PROTO]);
	plain[MELNODE_HDR_OFF_SRC] = READ_ONCE(inst->local_id);
	plain[MELNODE_HDR_OFF_DST] = nla_get_u8(info->attrs[MELNODE_A_PKT_DST]);
	plain[MELNODE_HDR_OFF_TTL] = ttl;
	if (payload_len)
		memcpy(plain + MELNODE_HDR_LEN, nla_data(data_attr), payload_len);

	err = melnode_routing_send_now(link, plain, plain_len);
	kfree(plain);
	if (err == -ENOENT)
		melnode_send_initiation(link, false);
	melnode_link_put(link);
	if (err)
		goto dropped;

	melnode_stat_inc(inst, MELNODE_STAT_INJECT_SENT);
	return 0;

dropped:
	melnode_stat_inc(inst, MELNODE_STAT_INJECT_DROPPED);
	return 0;
}

static void melnode_hs_enqueue(struct melnode_instance *inst, const u8 *data, size_t len,
			       const struct melnode_endpoint *from)
{
	struct melnode_hs_item *item;

	if (READ_ONCE(inst->hs_count) >= MELNODE_MAX_QUEUED_HANDSHAKES)
		return;

	item = kmalloc(sizeof(*item) + len, GFP_KERNEL);
	if (!item)
		return;
	item->from = *from;
	item->len = len;
	memcpy(item->data, data, len);

	spin_lock_bh(&inst->hs_lock);
	if (inst->hs_count >= MELNODE_MAX_QUEUED_HANDSHAKES) {
		spin_unlock_bh(&inst->hs_lock);
		kfree(item);
		return;
	}
	list_add_tail(&item->list, &inst->hs_queue);
	inst->hs_count++;
	spin_unlock_bh(&inst->hs_lock);
	queue_work(melnode_wq, &inst->hs_work);
}

static struct melnode_hs_item *melnode_hs_dequeue(struct melnode_instance *inst)
{
	struct melnode_hs_item *item;

	spin_lock_bh(&inst->hs_lock);
	item = list_first_entry_or_null(&inst->hs_queue, struct melnode_hs_item, list);
	if (item) {
		list_del(&item->list);
		inst->hs_count--;
	}
	spin_unlock_bh(&inst->hs_lock);
	return item;
}

static void melnode_hs_drain(struct melnode_instance *inst)
{
	struct melnode_hs_item *item;

	while ((item = melnode_hs_dequeue(inst)))
		kfree(item);
}

static bool melnode_under_load(struct melnode_instance *inst)
{
	u64 now = ktime_get_coarse_boottime_ns();

	if (READ_ONCE(inst->hs_count) >= MELNODE_MAX_QUEUED_HANDSHAKES / 8) {
		inst->last_under_load = now;
		return true;
	}
	return inst->last_under_load &&
	       (s64)(inst->last_under_load + SECS_TO_NS(MELNODE_UNDERLOAD_AFTER_TIME)) >
		       (s64)now;
}

static void melnode_handle_handshake_initiation(struct melnode_instance *inst, const struct melnode_hs_item *item)
{
	const struct melnode_wire_handshake_initiation *msg = (const void *)item->data;
	struct melnode_handshake_precursor pre;
	struct melnode_wire_handshake_response resp;
	struct melnode_link *link = NULL;
	u8 remote_static[32];
	u8 local_pub[32], local_priv[32];
	u32 index;
	bool ok;
	int i;

	melnode_get_keys(inst, local_pub, local_priv);
	ok = melnode_noise_handshake_consume_initiation1(msg, local_pub, local_priv, remote_static,
							 &pre);
	memzero_explicit(local_priv, sizeof(local_priv));
	if (!ok)
		goto out;

	rcu_read_lock();
	for (i = 0; i < MELNODE_MAX_PEER; i++) {
		struct melnode_link *cand = rcu_dereference(inst->links[i]);

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
	ok = melnode_noise_handshake_consume_initiation2(msg, &pre, &link->handshake);
	if (ok) {
		index = melnode_index_alloc(link);
		ok = index && melnode_noise_handshake_create_response(&resp, &link->handshake, index);
	}
	if (ok) {
		melnode_link_set_endpoint_from_packet_locked(link, &item->from);
		melnode_cookie_add_mac_to_packet(&resp, sizeof(resp), &link->cookie);
		melnode_noise_handshake_begin_session(&link->handshake, &link->keypairs);
		link->handshake.last_sent_initiation = ktime_get_coarse_boottime_ns();
		link->keepalive_at = 0;
		melnode_link_session_established_locked(link, false);
		atomic64_add(item->len, &link->rx_bytes);
	}
	melnode_index_sync(link);
	spin_unlock_bh(&link->lock);
	if (!ok)
		goto out;

	if (melnode_socket_send(inst, &resp, sizeof(resp), &item->from,
				 MELNODE_HANDSHAKE_DSCP) >= 0)
		atomic64_add(sizeof(resp), &link->tx_bytes);

out:
	if (link)
		melnode_link_put(link);
	memzero_explicit(&pre, sizeof(pre));
}

static void melnode_handle_handshake_response(struct melnode_instance *inst, const struct melnode_hs_item *item)
{
	const struct melnode_wire_handshake_response *msg = (const void *)item->data;
	struct melnode_link *link;
	u8 local_priv[32];
	bool ok;

	link = melnode_index_lookup(inst, msg->receiver_index);
	if (!link)
		return;

	melnode_get_keys(inst, NULL, local_priv);

	spin_lock_bh(&link->lock);
	ok = link->handshake.local_index == msg->receiver_index &&
	     melnode_noise_handshake_consume_response(msg, local_priv, &link->handshake);
	memzero_explicit(local_priv, sizeof(local_priv));
	if (ok) {
		melnode_link_set_endpoint_from_packet_locked(link, &item->from);
		melnode_noise_handshake_begin_session(&link->handshake, &link->keypairs);
		link->last_handshake_unix_ns = ktime_get_real_ns();
		link->keepalive_at = 0;
		melnode_link_session_established_locked(link, true);
		atomic64_add(item->len, &link->rx_bytes);
	}
	melnode_index_sync(link);
	spin_unlock_bh(&link->lock);
	if (!ok)
		goto out;

	melnode_routing_send_now(link, NULL, 0);
	pr_debug("melnode: link %u handshake complete\n", link->peer_id);

out:
	melnode_link_put(link);
}

static void melnode_handle_cookie_reply(struct melnode_instance *inst,
					const struct melnode_hs_item *item)
{
	const struct melnode_wire_handshake_cookie *msg = (const void *)item->data;
	struct melnode_link *link;

	link = melnode_index_lookup(inst, msg->receiver_index);
	if (!link)
		return;

	spin_lock_bh(&link->lock);
	melnode_cookie_message_consume(msg, &link->cookie);
	spin_unlock_bh(&link->lock);
	melnode_link_put(link);
}

static void melnode_handle_handshake(struct melnode_instance *inst, const struct melnode_hs_item *item)
{
	enum melnode_cookie_mac_state mac_state;
	bool under_load;
	__le32 type;

	memcpy(&type, item->data, sizeof(type));
	if (le32_to_cpu(type) == MELNODE_WIRE_MSG_HANDSHAKE_COOKIE) {
		melnode_handle_cookie_reply(inst, item);
		return;
	}

	under_load = melnode_under_load(inst);
	mac_state = melnode_cookie_validate_packet(&inst->cookie_checker, item->data,
						   item->len, item->from.addr,
						   item->from.addr_len, under_load);
	switch (mac_state) {
	case MELNODE_COOKIE_INVALID_MAC:
	case MELNODE_COOKIE_VALID_MAC_WITH_COOKIE_BUT_RATELIMITED:
		return;
	case MELNODE_COOKIE_VALID_MAC_BUT_NO_COOKIE:
		if (under_load) {
			struct melnode_wire_handshake_cookie reply;
			__le32 sender_index;

			memcpy(&sender_index, item->data + sizeof(struct melnode_wire_header),
			       sizeof(sender_index));
			melnode_cookie_message_create(&reply, item->data, item->len,
						      item->from.addr, item->from.addr_len,
						      sender_index, &inst->cookie_checker);
			melnode_socket_send(inst, &reply, sizeof(reply), &item->from, 0);
			return;
		}
		break;
	case MELNODE_COOKIE_VALID_MAC_WITH_COOKIE:
		break;
	}

	if (le32_to_cpu(type) == MELNODE_WIRE_MSG_HANDSHAKE_INITIATION)
		melnode_handle_handshake_initiation(inst, item);
	else
		melnode_handle_handshake_response(inst, item);
}

static void melnode_hs_work_fn(struct work_struct *work)
{
	struct melnode_instance *inst = container_of(work, struct melnode_instance, hs_work);
	struct melnode_hs_item *item;

	while ((item = melnode_hs_dequeue(inst))) {
		melnode_handle_handshake(inst, item);
		kfree(item);
		cond_resched();
	}
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

static void melnode_handle_data(struct melnode_instance *inst, u8 *data, size_t len, const struct melnode_endpoint *from)
{
	struct melnode_wire_data *msg = (void *)data;
	u8 recv_key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	struct melnode_keypair *kp;
	struct melnode_link *link;
	size_t cipher_len, plain_len;
	bool is_next, rekey = false, confirmed = false;
	u8 peer_id;
	u64 counter;
	u8 *plain;

	if (len < sizeof(*msg) + MELNODE_NOISE_AUTHTAG_LEN)
		return;
	cipher_len = len - sizeof(*msg);
	plain_len = cipher_len - MELNODE_NOISE_AUTHTAG_LEN;
	counter = le64_to_cpu(msg->counter);

	link = melnode_index_lookup(inst, msg->receiver_index);
	if (!link)
		return;
	peer_id = link->peer_id;

	spin_lock_bh(&link->lock);
	kp = melnode_find_keypair(link, msg->receiver_index, &is_next);
	if (!kp || !kp->receiving.is_valid ||
	    melnode_key_expired(&kp->receiving, MELNODE_REJECT_AFTER_TIME)) {
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
	if (is_next && melnode_noise_received_with_keypair(&link->keypairs)) {
		kp = &link->keypairs.current_kp;
		link->last_handshake_unix_ns = ktime_get_real_ns();
		melnode_link_session_established_locked(link, true);
		confirmed = true;
	}
	melnode_link_set_endpoint_from_packet_locked(link, from);
	if (!link->sent_last_minute_handshake && kp == &link->keypairs.current_kp &&
	    kp->initiator &&
	    melnode_key_expired(&kp->sending, MELNODE_REJECT_AFTER_TIME - MELNODE_KEEPALIVE_TIMEOUT -
						      MELNODE_REKEY_TIMEOUT)) {
		link->sent_last_minute_handshake = true;
		rekey = true;
	}
	atomic64_add(len, &link->rx_bytes);
	link->new_handshake_at = 0;
	if (plain_len) {
		if (link->keepalive_at) {
			link->need_another_keepalive = true;
		} else {
			link->keepalive_at = ktime_get_coarse_boottime_ns() +
					     SECS_TO_NS(MELNODE_KEEPALIVE_TIMEOUT);
			melnode_link_arm_timer_locked(link);
		}
	}
	melnode_index_sync(link);
	spin_unlock_bh(&link->lock);
	if (confirmed)
		pr_debug("melnode: link %u session confirmed\n", peer_id);
	if (rekey)
		melnode_send_initiation(link, false);
	melnode_link_put(link);

	if (!plain_len) {
		kfree(plain);
		return;
	}
	if (plain_len < MELNODE_HDR_LEN) {
		kfree(plain);
		melnode_stat_inc(inst, MELNODE_STAT_RX_BAD_PACKET);
		return;
	}

	if (plain[MELNODE_HDR_OFF_PROTO] != 0) {
		melnode_send_punt(inst, peer_id, plain[MELNODE_HDR_OFF_PROTO], plain[MELNODE_HDR_OFF_SRC],
				  plain[MELNODE_HDR_OFF_DST], plain[MELNODE_HDR_OFF_TTL],
				  plain + MELNODE_HDR_LEN, plain_len - MELNODE_HDR_LEN);
		kfree(plain);
		return;
	}

	melnode_routing_deliver_or_forward(inst, plain[MELNODE_HDR_OFF_SRC], plain[MELNODE_HDR_OFF_DST],
					   plain[MELNODE_HDR_OFF_TTL], plain, plain_len);
	return;

bad_packet:
	melnode_link_put(link);
	melnode_stat_inc(inst, MELNODE_STAT_RX_BAD_PACKET);
}

void melnode_handle_datagram(struct melnode_instance *inst, u8 *data, size_t len,
			     const struct melnode_endpoint *from)
{
	__le32 type;

	if (len < sizeof(struct melnode_wire_header))
		return;
	memcpy(&type, data, sizeof(type));

	switch (le32_to_cpu(type)) {
	case MELNODE_WIRE_MSG_HANDSHAKE_INITIATION:
		if (len == sizeof(struct melnode_wire_handshake_initiation))
			melnode_hs_enqueue(inst, data, len, from);
		break;
	case MELNODE_WIRE_MSG_HANDSHAKE_RESPONSE:
		if (len == sizeof(struct melnode_wire_handshake_response))
			melnode_hs_enqueue(inst, data, len, from);
		break;
	case MELNODE_WIRE_MSG_HANDSHAKE_COOKIE:
		if (len == sizeof(struct melnode_wire_handshake_cookie))
			melnode_hs_enqueue(inst, data, len, from);
		break;
	case MELNODE_WIRE_MSG_DATA:
		melnode_handle_data(inst, data, len, from);
		break;
	}
}

static int melnode_pre_doit(const struct genl_split_ops *ops, struct sk_buff *skb,
			    struct genl_info *info)
{
	struct melnode_instance *inst;

	info->user_ptr[0] = NULL;
	if (melnode_compat_genl_cmd(ops) == MELNODE_CMD_DEVICE_SET)
		return 0;

	inst = melnode_instance_get(genl_info_net(info), info->snd_portid);
	if (!inst)
		return -ENODEV;
	info->user_ptr[0] = inst;
	return 0;
}

static void melnode_post_doit(const struct genl_split_ops *ops, struct sk_buff *skb,
			      struct genl_info *info)
{
	if (info->user_ptr[0])
		melnode_instance_put(info->user_ptr[0]);
}

static const struct genl_ops melnode_ops[] = {
	{ .cmd = MELNODE_CMD_DEVICE_SET, .doit = melnode_nl_device_set, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_STATS_GET, .dumpit = melnode_nl_stats_get, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_LINK_SET, .doit = melnode_nl_link_set, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_LINK_DEL, .doit = melnode_nl_link_del, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_LINK_GET, .dumpit = melnode_nl_link_get, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_ROUTE_SET, .doit = melnode_nl_route_set, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_ROUTE_GET, .dumpit = melnode_nl_route_get, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_INJECT, .doit = melnode_nl_inject, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_TUN_SET, .doit = melnode_nl_tun_set, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_TUN_DEL, .doit = melnode_nl_tun_del, .flags = GENL_ADMIN_PERM },
	{ .cmd = MELNODE_CMD_TUN_GET, .dumpit = melnode_nl_tun_get, .flags = GENL_ADMIN_PERM },
};

static struct genl_family melnode_genl_family __ro_after_init = {
	.name = MELNODE_GENL_NAME,
	.version = MELNODE_GENL_VERSION,
	.maxattr = MELNODE_A_MAX,
	.policy = melnode_policy,
	.module = THIS_MODULE,
	.pre_doit = melnode_pre_doit,
	.post_doit = melnode_post_doit,
	.ops = melnode_ops,
	.n_ops = ARRAY_SIZE(melnode_ops),
	.parallel_ops = true,
};

static int __init melnode_init(void)
{
	int err;

	melnode_noise_init();

	err = melnode_ratelimiter_module_init();
	if (err)
		return err;

	melnode_wq = alloc_workqueue("melnode", WQ_UNBOUND, 0);
	if (!melnode_wq) {
		err = -ENOMEM;
		goto err_ratelimiter;
	}
	melnode_destroy_wq = alloc_ordered_workqueue("melnode_destroy", 0);
	if (!melnode_destroy_wq) {
		err = -ENOMEM;
		goto err_wq;
	}

	err = rtnl_link_register(&melnode_link_ops);
	if (err)
		goto err_destroy_wq;

	err = genl_register_family(&melnode_genl_family);
	if (err)
		goto err_link_ops;

	err = netlink_register_notifier(&melnode_netlink_notifier);
	if (err)
		goto err_family;

	pr_info("melnode: family \"%s\" v%d registered\n", MELNODE_GENL_NAME, MELNODE_GENL_VERSION);
	return 0;

err_family:
	genl_unregister_family(&melnode_genl_family);
err_link_ops:
	rtnl_link_unregister(&melnode_link_ops);
err_destroy_wq:
	destroy_workqueue(melnode_destroy_wq);
err_wq:
	destroy_workqueue(melnode_wq);
err_ratelimiter:
	melnode_ratelimiter_module_exit();
	return err;
}

static void __exit melnode_exit(void)
{
	struct melnode_instance *inst;

	netlink_unregister_notifier(&melnode_netlink_notifier);
	genl_unregister_family(&melnode_genl_family);
	flush_workqueue(melnode_destroy_wq);

	mutex_lock(&melnode_instances_lock);
	while ((inst = list_first_entry_or_null(&melnode_instances, struct melnode_instance,
						list))) {
		list_del_init(&inst->list);
		mutex_unlock(&melnode_instances_lock);
		melnode_instance_destroy(inst);
		mutex_lock(&melnode_instances_lock);
	}
	mutex_unlock(&melnode_instances_lock);

	rtnl_link_unregister(&melnode_link_ops);
	destroy_workqueue(melnode_destroy_wq);
	destroy_workqueue(melnode_wq);
	melnode_ratelimiter_module_exit();
	pr_info("melnode: unloaded\n");
}

module_init(melnode_init);
module_exit(melnode_exit);

MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("melnode kernel data plane (genl family \"melnode\")");
MODULE_VERSION(__stringify(MELNODE_GENL_VERSION));
MODULE_ALIAS_RTNL_LINK(MELNODE_LINK_KIND);
MODULE_SOFTDEP("pre: libcurve25519 libchacha20poly1305");
