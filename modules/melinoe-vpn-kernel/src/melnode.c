// SPDX-License-Identifier: GPL-2.0-only
/*
 * melnode kernel data plane.
 *
 * Implements the "melnode" generic netlink family (dpproto/API.md,
 * melnode_genl.h) as an in-kernel alternative to melnode-dp. melnode-cp is
 * unaware which implementation it's talking to.
 *
 * Current scope: device lifecycle (HELLO/ATTACH/DEVICE_SET/DEVICE_DEL),
 * stats, and link/route bookkeeping. NOT yet implemented: tun netdevs
 * (TUN_CREATE/START/DESTROY/GET), the packet path (INJECT/PUNT/EVENT), and
 * therefore any actual forwarding or Noise handshake - DEVICE_SET does
 * derive a real X25519 public key (matching the userspace data plane's and
 * WireGuard's key format) since that much is self-contained, but no session
 * is established. A peer's LINK_ADD is accepted and stored, but nothing yet
 * sends or receives traffic on it.
 *
 * The genl scaffolding (family/ops layout, the ATTACH portid + netlink
 * release-notifier pattern, the dump-with-cb->args idiom, the
 * nla_put_u64_64bit usage) follows drivers/net/wireguard/{device,netlink}.c
 * (Jason A. Donenfeld et al., GPL-2.0), which solves the same
 * "one genl family, one kernel-resident peer/device table" problem this
 * does. No code was copied verbatim; the attribute/command set here is
 * melnode's own, defined by melnode_genl.h.
 */

#include <linux/module.h>
#include <linux/init.h>
#include <linux/mutex.h>
#include <linux/netlink.h>
#include <linux/in.h>
#include <linux/in6.h>
#include <net/genetlink.h>
#include <crypto/curve25519.h>

#include "melnode_genl.h"

#define MELNODE_A_MAX MELNODE_A_EVT_TIME
#define MELNODE_MAX_PEER 256 /* peer/node/dst ids are one byte on the wire */

/* ------------------------------------------------------------------------
 * Device state. One device for the whole module (init_net only) for now;
 * see API.md's kernel notes on one-device-per-netns being the eventual
 * target.
 * ------------------------------------------------------------------------ */

struct melnode_link {
	bool valid;
	u8 public_key[32];
	bool has_endpoint;
	u8 endpoint[sizeof(struct sockaddr_in6)];
	u8 endpoint_len;
	u64 last_handshake; /* always 0: no crypto session yet */
	atomic64_t tx_bytes;
	atomic64_t rx_bytes;
};

struct melnode_route {
	bool valid;
	u8 nexthop; /* peer id */
};

struct melnode_device {
	struct mutex lock;
	bool configured;

	u32 local_id;
	u8 private_key[32];
	u8 public_key[32];
	u16 listen_port;
	u32 mtu;
	u32 fwmark;

	u32 attached_portid; /* 0 = nobody attached */

	struct melnode_link links[MELNODE_MAX_PEER];
	struct melnode_route routes[MELNODE_MAX_PEER];

	atomic64_t stats[MELNODE_STAT_RX_QUEUE_FULL + 1];
};

static struct melnode_device melnode_dev = {
	.lock = __MUTEX_INITIALIZER(melnode_dev.lock),
};

static void melnode_stat_inc(enum melnode_stat id)
{
	if (id > 0 && id <= MELNODE_STAT_RX_QUEUE_FULL)
		atomic64_inc(&melnode_dev.stats[id]);
}

static bool melnode_configured(void)
{
	return READ_ONCE(melnode_dev.configured);
}

/* 0..255 attribute helper: on the wire these are NLA_U32 per
 * melnode_genl.h, but only the low byte is meaningful (peer/node/dst ids
 * are one byte).
 */
static int melnode_get_u8_id(struct nlattr *attr, u8 *out)
{
	u32 v;

	if (!attr)
		return -EINVAL;
	v = nla_get_u32(attr);
	if (v > 255)
		return -EINVAL;
	*out = (u8)v;
	return 0;
}

/* ------------------------------------------------------------------------
 * Generic netlink family
 * ------------------------------------------------------------------------ */

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
};

/* ------------------------------------------------------------------------
 * HELLO
 * ------------------------------------------------------------------------ */

static int melnode_nl_hello(struct sk_buff *skb, struct genl_info *info)
{
	struct sk_buff *reply;
	void *hdr;
	u32 version;

	if (!info->attrs[MELNODE_A_API_VERSION])
		return -EINVAL;
	version = nla_get_u32(info->attrs[MELNODE_A_API_VERSION]);
	if (version != MELNODE_GENL_VERSION)
		return -EOPNOTSUPP;

	reply = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_KERNEL);
	if (!reply)
		return -ENOMEM;

	hdr = genlmsg_put_reply(reply, info, &melnode_genl_family, 0, MELNODE_CMD_HELLO);
	if (!hdr)
		goto nla_put_failure;

	if (nla_put_u32(reply, MELNODE_A_API_VERSION, MELNODE_GENL_VERSION) ||
	    nla_put_u8(reply, MELNODE_A_CONFIGURED, melnode_configured()))
		goto nla_put_failure;

	genlmsg_end(reply, hdr);
	return genlmsg_reply(reply, info);

nla_put_failure:
	nlmsg_free(reply);
	return -EMSGSIZE;
}

/* ------------------------------------------------------------------------
 * ATTACH - this socket becomes the control plane (punts + events), like
 * OVS's upcall pid (API.md). Detaching (socket closed) leaves forwarding
 * running.
 * ------------------------------------------------------------------------ */

static int melnode_nl_attach(struct sk_buff *skb, struct genl_info *info)
{
	mutex_lock(&melnode_dev.lock);
	melnode_dev.attached_portid = info->snd_portid;
	mutex_unlock(&melnode_dev.lock);
	return 0;
}

static int melnode_netlink_notifier_call(struct notifier_block *nb, unsigned long state,
					  void *_notify)
{
	struct netlink_notify *notify = _notify;

	if (state != NETLINK_URELEASE || notify->protocol != NETLINK_GENERIC)
		return NOTIFY_DONE;

	mutex_lock(&melnode_dev.lock);
	if (melnode_dev.attached_portid == notify->portid)
		melnode_dev.attached_portid = 0;
	mutex_unlock(&melnode_dev.lock);
	return NOTIFY_DONE;
}

static struct notifier_block melnode_netlink_notifier = {
	.notifier_call = melnode_netlink_notifier_call,
};

/* ------------------------------------------------------------------------
 * DEVICE_SET / DEVICE_DEL
 * ------------------------------------------------------------------------ */

static int melnode_nl_device_set(struct sk_buff *skb, struct genl_info *info)
{
	struct sk_buff *reply;
	void *hdr;
	u32 local_id;
	u8 private_key[32];
	u8 public_key[32];
	u16 listen_port;
	u32 mtu;
	u32 fwmark = 0;
	int err;

	if (!info->attrs[MELNODE_A_LOCAL_ID] || !info->attrs[MELNODE_A_PRIVATE_KEY] ||
	    !info->attrs[MELNODE_A_LISTEN_PORT] || !info->attrs[MELNODE_A_MTU])
		return -EINVAL;

	local_id = nla_get_u32(info->attrs[MELNODE_A_LOCAL_ID]);
	if (local_id > 255)
		return -EINVAL;
	listen_port = nla_get_u16(info->attrs[MELNODE_A_LISTEN_PORT]);
	mtu = nla_get_u32(info->attrs[MELNODE_A_MTU]);
	if (mtu < 576 || mtu > 65000)
		return -EINVAL;
	if (info->attrs[MELNODE_A_FWMARK])
		fwmark = nla_get_u32(info->attrs[MELNODE_A_FWMARK]);
	memcpy(private_key, nla_data(info->attrs[MELNODE_A_PRIVATE_KEY]), 32);

	/* As in wg_set_device(): compute the public key before taking the
	 * device lock, and reject a degenerate (all-zero) key outright.
	 */
	if (!curve25519_generate_public(public_key, private_key)) {
		memzero_explicit(private_key, sizeof(private_key));
		return -EINVAL;
	}

	mutex_lock(&melnode_dev.lock);

	if (melnode_dev.configured) {
		bool identical = melnode_dev.local_id == local_id &&
				  melnode_dev.listen_port == listen_port &&
				  melnode_dev.mtu == mtu && melnode_dev.fwmark == fwmark &&
				  !memcmp(melnode_dev.private_key, private_key, 32);

		mutex_unlock(&melnode_dev.lock);
		memzero_explicit(private_key, sizeof(private_key));
		if (identical)
			goto reply_pubkey;
		return -EEXIST;
	}

	melnode_dev.local_id = local_id;
	memcpy(melnode_dev.private_key, private_key, 32);
	memcpy(melnode_dev.public_key, public_key, 32);
	melnode_dev.listen_port = listen_port;
	melnode_dev.mtu = mtu;
	melnode_dev.fwmark = fwmark;
	/* TODO: bind the UDP socket and start the crypto/forwarding path
	 * (currently just bookkeeping - see the file header).
	 */
	WRITE_ONCE(melnode_dev.configured, true);

	mutex_unlock(&melnode_dev.lock);
	memzero_explicit(private_key, sizeof(private_key));

reply_pubkey:
	reply = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_KERNEL);
	if (!reply)
		return -ENOMEM;
	hdr = genlmsg_put_reply(reply, info, &melnode_genl_family, 0, MELNODE_CMD_DEVICE_SET);
	if (!hdr) {
		nlmsg_free(reply);
		return -EMSGSIZE;
	}
	err = nla_put(reply, MELNODE_A_PUBLIC_KEY, 32, public_key);
	if (err) {
		nlmsg_free(reply);
		return -EMSGSIZE;
	}
	genlmsg_end(reply, hdr);
	return genlmsg_reply(reply, info);
}

static int melnode_nl_device_del(struct sk_buff *skb, struct genl_info *info)
{
	mutex_lock(&melnode_dev.lock);
	/* TODO: tear down tuns/routes/sockets once they exist. */
	memset(melnode_dev.links, 0, sizeof(melnode_dev.links));
	memset(melnode_dev.routes, 0, sizeof(melnode_dev.routes));
	memzero_explicit(melnode_dev.private_key, sizeof(melnode_dev.private_key));
	memset(melnode_dev.public_key, 0, sizeof(melnode_dev.public_key));
	WRITE_ONCE(melnode_dev.configured, false);
	mutex_unlock(&melnode_dev.lock);
	return 0;
}

/* ------------------------------------------------------------------------
 * STATS_GET (dump)
 * ------------------------------------------------------------------------ */

static int melnode_nl_stats_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	int id = cb->args[0];

	if (id == 0)
		id = 1; /* MELNODE_STAT_* ids start at 1; 0 is "unspec". */

	for (; id <= MELNODE_STAT_RX_QUEUE_FULL; id++) {
		void *hdr;
		s64 value = atomic64_read(&melnode_dev.stats[id]);

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

/* ------------------------------------------------------------------------
 * LINK_ADD / LINK_DEL / LINK_GET
 * ------------------------------------------------------------------------ */

static int melnode_nl_link_add(struct sk_buff *skb, struct genl_info *info)
{
	u8 peer_id;
	struct melnode_link *link;
	const u8 *pubkey;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;
	if (!info->attrs[MELNODE_A_PUBLIC_KEY])
		return -EINVAL;
	pubkey = nla_data(info->attrs[MELNODE_A_PUBLIC_KEY]);

	mutex_lock(&melnode_dev.lock);
	link = &melnode_dev.links[peer_id];

	if (link->valid && memcmp(link->public_key, pubkey, 32)) {
		mutex_unlock(&melnode_dev.lock);
		return -EEXIST;
	}

	memcpy(link->public_key, pubkey, 32);
	link->valid = true;

	if (info->attrs[MELNODE_A_ENDPOINT]) {
		int len = nla_len(info->attrs[MELNODE_A_ENDPOINT]);

		memcpy(link->endpoint, nla_data(info->attrs[MELNODE_A_ENDPOINT]), len);
		link->endpoint_len = len;
		link->has_endpoint = true;
	}
	/* else: leave whatever endpoint we already had, per API.md (a
	 * listen-only link learns its peer's address from traffic).
	 */

	mutex_unlock(&melnode_dev.lock);
	return 0;
}

static int melnode_nl_link_del(struct sk_buff *skb, struct genl_info *info)
{
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	mutex_lock(&melnode_dev.lock);
	if (!melnode_dev.links[peer_id].valid) {
		mutex_unlock(&melnode_dev.lock);
		return -ENOENT;
	}
	memset(&melnode_dev.links[peer_id], 0, sizeof(melnode_dev.links[peer_id]));
	mutex_unlock(&melnode_dev.lock);
	/* Routes pointing at this peer are left alone; the control plane
	 * removes them, per API.md.
	 */
	return 0;
}

static int melnode_nl_link_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	int peer_id = cb->args[0];

	for (; peer_id < MELNODE_MAX_PEER; peer_id++) {
		struct melnode_link *link = &melnode_dev.links[peer_id];
		void *hdr;

		if (!link->valid)
			continue;

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				   &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_LINK_GET);
		if (!hdr)
			break;

		if (nla_put_u32(skb, MELNODE_A_PEER_ID, peer_id) ||
		    nla_put(skb, MELNODE_A_PUBLIC_KEY, 32, link->public_key) ||
		    (link->has_endpoint &&
		     nla_put(skb, MELNODE_A_ENDPOINT, link->endpoint_len, link->endpoint)) ||
		    nla_put_u64_64bit(skb, MELNODE_A_LAST_HANDSHAKE, link->last_handshake,
				       MELNODE_A_PAD) ||
		    nla_put_u64_64bit(skb, MELNODE_A_TX_BYTES, atomic64_read(&link->tx_bytes),
				       MELNODE_A_PAD) ||
		    nla_put_u64_64bit(skb, MELNODE_A_RX_BYTES, atomic64_read(&link->rx_bytes),
				       MELNODE_A_PAD)) {
			genlmsg_cancel(skb, hdr);
			break;
		}
		genlmsg_end(skb, hdr);
	}

	cb->args[0] = peer_id;
	return skb->len;
}

/* ------------------------------------------------------------------------
 * ROUTE_SET / ROUTE_DEL / ROUTE_GET
 * ------------------------------------------------------------------------ */

static int melnode_nl_route_set(struct sk_buff *skb, struct genl_info *info)
{
	u8 dst, nexthop;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_ROUTE_DST], &dst);
	if (err)
		return err;
	err = melnode_get_u8_id(info->attrs[MELNODE_A_ROUTE_NEXTHOP], &nexthop);
	if (err)
		return err;

	mutex_lock(&melnode_dev.lock);
	melnode_dev.routes[dst].valid = true;
	melnode_dev.routes[dst].nexthop = nexthop;
	mutex_unlock(&melnode_dev.lock);
	return 0;
}

static int melnode_nl_route_del(struct sk_buff *skb, struct genl_info *info)
{
	u8 dst;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_ROUTE_DST], &dst);
	if (err)
		return err;

	mutex_lock(&melnode_dev.lock);
	melnode_dev.routes[dst].valid = false;
	mutex_unlock(&melnode_dev.lock);
	return 0;
}

static int melnode_nl_route_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	int dst = cb->args[0];

	for (; dst < MELNODE_MAX_PEER; dst++) {
		struct melnode_route *route = &melnode_dev.routes[dst];
		void *hdr;

		if (!route->valid)
			continue;

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				   &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_ROUTE_GET);
		if (!hdr)
			break;
		if (nla_put_u32(skb, MELNODE_A_ROUTE_DST, dst) ||
		    nla_put_u32(skb, MELNODE_A_ROUTE_NEXTHOP, route->nexthop)) {
			genlmsg_cancel(skb, hdr);
			break;
		}
		genlmsg_end(skb, hdr);
	}

	cb->args[0] = dst;
	return skb->len;
}

/* ------------------------------------------------------------------------
 * INJECT - needs the packet path, not implemented yet.
 * ------------------------------------------------------------------------ */

static int melnode_nl_inject(struct sk_buff *skb, struct genl_info *info)
{
	melnode_stat_inc(MELNODE_STAT_INJECT_DROPPED);
	return -EOPNOTSUPP;
}

/* ------------------------------------------------------------------------
 * Pre-flight gating: everything but HELLO/ATTACH/DEVICE_SET/DEVICE_DEL/
 * STATS_GET requires a configured device (API.md).
 * ------------------------------------------------------------------------ */

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
	{
		.cmd = MELNODE_CMD_HELLO,
		.doit = melnode_nl_hello,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_ATTACH,
		.doit = melnode_nl_attach,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_DEVICE_SET,
		.doit = melnode_nl_device_set,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_DEVICE_DEL,
		.doit = melnode_nl_device_del,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_STATS_GET,
		.dumpit = melnode_nl_stats_get,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_LINK_ADD,
		.doit = melnode_nl_link_add,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_LINK_DEL,
		.doit = melnode_nl_link_del,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_LINK_GET,
		.dumpit = melnode_nl_link_get,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_ROUTE_SET,
		.doit = melnode_nl_route_set,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_ROUTE_DEL,
		.doit = melnode_nl_route_del,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_ROUTE_GET,
		.dumpit = melnode_nl_route_get,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_INJECT,
		.doit = melnode_nl_inject,
		.flags = GENL_ADMIN_PERM,
	},
	/* TUN_CREATE/START/DESTROY/GET: not yet implemented (needs real
	 * netdev plumbing) - falls through to "unknown command" -EOPNOTSUPP,
	 * which is a documented API.md error code.
	 *
	 * X_QUIT: deliberately unregistered. "Userspace only" per API.md; a
	 * kernel data plane has no process to exit, and unknown commands
	 * already get EOPNOTSUPP.
	 */
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
};

static int __init melnode_init(void)
{
	int err;

	err = genl_register_family(&melnode_genl_family);
	if (err)
		return err;

	err = netlink_register_notifier(&melnode_netlink_notifier);
	if (err) {
		genl_unregister_family(&melnode_genl_family);
		return err;
	}

	pr_info("melnode: family \"%s\" v%d registered\n", MELNODE_GENL_NAME,
		MELNODE_GENL_VERSION);
	return 0;
}

static void __exit melnode_exit(void)
{
	netlink_unregister_notifier(&melnode_netlink_notifier);
	genl_unregister_family(&melnode_genl_family);
	pr_info("melnode: unloaded\n");
}

module_init(melnode_init);
module_exit(melnode_exit);

MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("melnode kernel data plane (genl family \"melnode\")");
/* curve25519_generate_public() lives in lib/crypto/curve25519.c, built as
 * its own module (CONFIG_CRYPTO_LIB_CURVE25519=m on most distro kernels,
 * confirmed on the 7.2.7 test target). `modprobe melnode` resolves this via
 * the normal symbol-dependency path in modules.dep; this is just so
 * `modinfo melnode` documents it and depmod has an explicit hint rather
 * than only the implicit undefined-symbol one. `insmod` does NOT resolve
 * this - load libcurve25519 first if testing with insmod directly.
 */
MODULE_SOFTDEP("pre: libcurve25519");
