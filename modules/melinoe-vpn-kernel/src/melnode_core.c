// SPDX-License-Identifier: GPL-2.0-only
/*
 * melnode kernel data plane.
 *
 * Implements the "melnode" generic netlink family (dpproto/API.md,
 * melnode_genl.h) as an in-kernel alternative to melnode-dp. melnode-cp is
 * unaware which implementation it's talking to.
 *
 * Full scope now: device/link/route/tun lifecycle, the Noise_IKpsk2
 * handshake and mac1/mac2 cookie-reply DoS mitigation (melnode_noise.c/
 * melnode_cookie.c/melnode_ratelimiter.c), the UDP socket
 * (melnode_socket.c), and the actual forwarding path: tun -> encrypt ->
 * link, link -> decrypt -> local tun or re-encrypt onto the next hop
 * (decrementing TTL), proto != 0 -> PUNT. This is melnode's own routing
 * semantics (a 4-byte [proto,src,dst,ttl] header inside the decrypted
 * payload, one point-to-point tun per peer id, multi-hop store-and-
 * forward via the route table) - see melnode-dp's router.go/ctl.go for the
 * exact byte layout this must match.
 *
 * Session indices: rather than WireGuard's device-wide index hashtable
 * (needed for its ~2^20-peer, fully-random-index design), a melnode index
 * is (random bits << 8) | peer_id, where peer_id is *whoever generated the
 * index's own* link-table key for the other party - see melnode_noise.h's
 * header comment. Both create_initiation and consume_initiation2 (i.e.
 * both the initiator's and the responder's own index) use
 * melnode_gen_index(peer_id) for exactly this reason: an index is only
 * ever decoded by whoever created it, indexing their own table.
 *
 * Deliberate simplifications, all documented where they matter:
 *  - No staged-packet queue: a tun xmit while no valid session exists
 *    triggers a handshake and drops that one packet, rather than WireGuard's
 *    queue-and-flush-on-handshake-complete.
 *  - Cookie/mac2 proof is *always* required for handshake initiations (see
 *    melnode_handle_datagram), not adaptively under WireGuard's queue-depth
 *    "under load" heuristic - a strictly stronger DoS posture, and free
 *    because melnode-dp (unmodified wireguard-go) already retries with a
 *    cookie on receiving a cookie-reply.
 *  - Linear-buffer crypto (melnode_transport.c) instead of scatterlists.
 *  - Single melnode_dev.lock instead of WireGuard's per-object rwsem/kref/
 *    RCU (see melnode_noise.h).
 *
 * The genl scaffolding (family/ops layout, the ATTACH portid + netlink
 * release-notifier pattern, the dump-with-cb->args idiom, the
 * nla_put_u64_64bit usage) follows drivers/net/wireguard/{device,netlink}.c
 * (Jason A. Donenfeld et al., GPL-2.0); the handshake/cookie/transport
 * modules this file drives are ported more directly from WireGuard's
 * noise.c/cookie.c/ratelimiter.c/receive.c/send.c - see those files'
 * headers for exactly what's verbatim versus adapted. The attribute/
 * command set and the routing-header wire format are melnode's own,
 * defined by melnode_genl.h and melnode-dp respectively.
 */

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
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/list.h>
#include <linux/netdevice.h>
#include <linux/rtnetlink.h>
#include <net/genetlink.h>
#include <crypto/curve25519.h>

#include "melnode_core.h"
#include "melnode_socket.h"
#include "melnode_transport.h"
#include "melnode_ratelimiter.h"

#define MELNODE_A_MAX MELNODE_A_EVT_TIME

/* melnode's own routing header, living inside the decrypted Noise
 * transport payload - NOT part of melnode_messages.h, since that file is
 * the Noise/wire-framing layer ported from WireGuard, while this is
 * melnode-dp's own addition on top of it. See router.go/ctl.go: byte 0
 * proto, byte 1 src peer id, byte 2 dst peer id, byte 3 ttl.
 */
#define MELNODE_HDR_LEN 4
#define MELNODE_HDR_OFF_PROTO 0
#define MELNODE_HDR_OFF_SRC 1
#define MELNODE_HDR_OFF_DST 2
#define MELNODE_HDR_OFF_TTL 3
#define MELNODE_DEFAULT_TTL 64

struct melnode_device melnode_dev = {
	.lock = __SPIN_LOCK_UNLOCKED(melnode_dev.lock),
	.pending_teardown_lock = __SPIN_LOCK_UNLOCKED(melnode_dev.pending_teardown_lock),
	.pending_teardown = LIST_HEAD_INIT(melnode_dev.pending_teardown),
};

struct melnode_pending_teardown {
	struct list_head list;
	struct net_device *dev;
};

/* See melnode_core.h's pending_teardown comment: this is where the
 * potentially-stalling half of tun destruction actually happens, off the
 * genl doit call's (and therefore melnode-cp's control channel's) critical
 * path.
 */
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
		/* Fall back to the old synchronous behaviour - better a stalled
		 * genl call than a leaked net_device.
		 */
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

/* See the file header: an index's low byte is always the peer_id in
 * whichever link table its creator will use to look it back up.
 */
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
	[MELNODE_A_TUN_NAME] = { .type = NLA_NUL_STRING, .len = IFNAMSIZ - 1 },
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
	spin_lock_bh(&melnode_dev.lock);
	melnode_dev.attached_portid = info->snd_portid;
	spin_unlock_bh(&melnode_dev.lock);
	return 0;
}

static int melnode_netlink_notifier_call(struct notifier_block *nb, unsigned long state,
					  void *_notify)
{
	struct netlink_notify *notify = _notify;

	if (state != NETLINK_URELEASE || notify->protocol != NETLINK_GENERIC)
		return NOTIFY_DONE;

	spin_lock_bh(&melnode_dev.lock);
	if (melnode_dev.attached_portid == notify->portid)
		melnode_dev.attached_portid = 0;
	spin_unlock_bh(&melnode_dev.lock);
	return NOTIFY_DONE;
}

static struct notifier_block melnode_netlink_notifier = {
	.notifier_call = melnode_netlink_notifier_call,
};

/* Unicasts PUNT to the attached control plane, if any. skb is consumed. */
static void melnode_send_punt(u8 ingress_link, u8 proto, u8 src, u8 dst, u8 ttl, const u8 *payload,
			       size_t payload_len)
{
	struct sk_buff *skb;
	void *hdr;
	u32 portid;

	portid = READ_ONCE(melnode_dev.attached_portid);
	if (!portid) {
		melnode_stat_inc(MELNODE_STAT_PUNT_DROPPED);
		return;
	}

	skb = genlmsg_new(NLMSG_DEFAULT_SIZE + payload_len, GFP_ATOMIC);
	if (!skb) {
		melnode_stat_inc(MELNODE_STAT_PUNT_DROPPED);
		return;
	}
	hdr = genlmsg_put(skb, 0, 0, &melnode_genl_family, 0, MELNODE_CMD_PUNT);
	if (!hdr)
		goto drop;
	if (nla_put_u32(skb, MELNODE_A_PKT_LINK, ingress_link) ||
	    nla_put_u8(skb, MELNODE_A_PKT_PROTO, proto) || nla_put_u8(skb, MELNODE_A_PKT_SRC, src) ||
	    nla_put_u8(skb, MELNODE_A_PKT_DST, dst) || nla_put_u8(skb, MELNODE_A_PKT_TTL, ttl) ||
	    nla_put(skb, MELNODE_A_PKT_DATA, payload_len, payload))
		goto drop;
	genlmsg_end(skb, hdr);
	if (genlmsg_unicast(&init_net, skb, portid)) {
		melnode_stat_inc(MELNODE_STAT_PUNT_DROPPED);
		return;
	}
	melnode_stat_inc(MELNODE_STAT_PUNT_SENT);
	return;

drop:
	nlmsg_free(skb);
	melnode_stat_inc(MELNODE_STAT_PUNT_DROPPED);
}

/* Multicasts EVENT (kind 1: handshake completed) to the "events" group. */
static void melnode_send_event_handshake(u8 peer_id, const void *endpoint, int endpoint_len)
{
	struct sk_buff *skb;
	void *hdr;

	skb = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_ATOMIC);
	if (!skb)
		return;
	hdr = genlmsg_put(skb, 0, 0, &melnode_genl_family, 0, MELNODE_CMD_EVENT);
	if (!hdr)
		goto drop;
	if (nla_put_u8(skb, MELNODE_A_EVT_KIND, MELNODE_EVENT_LINK_HANDSHAKE) ||
	    nla_put_u32(skb, MELNODE_A_PEER_ID, peer_id) ||
	    nla_put_u64_64bit(skb, MELNODE_A_EVT_TIME, ktime_get_real_ns(), MELNODE_A_PAD) ||
	    (endpoint && nla_put(skb, MELNODE_A_ENDPOINT, endpoint_len, endpoint)))
		goto drop;
	genlmsg_end(skb, hdr);
	if (genlmsg_multicast(&melnode_genl_family, skb, 0, 0, GFP_ATOMIC))
		melnode_stat_inc(MELNODE_STAT_EVENTS_DROPPED);
	return;

drop:
	nlmsg_free(skb);
	melnode_stat_inc(MELNODE_STAT_EVENTS_DROPPED);
}

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

	spin_lock_bh(&melnode_dev.lock);

	if (melnode_dev.configured) {
		bool identical = melnode_dev.local_id == local_id &&
				  melnode_dev.listen_port == listen_port &&
				  melnode_dev.mtu == mtu && melnode_dev.fwmark == fwmark &&
				  !memcmp(melnode_dev.private_key, private_key, 32);

		spin_unlock_bh(&melnode_dev.lock);
		memzero_explicit(private_key, sizeof(private_key));
		if (identical)
			goto reply_pubkey;
		return -EEXIST;
	}
	spin_unlock_bh(&melnode_dev.lock);

	/* Bind the socket before committing any state, so a bind failure
	 * (port in use, etc.) leaves the device cleanly unconfigured.
	 */
	err = melnode_socket_open(&melnode_dev, listen_port);
	if (err) {
		memzero_explicit(private_key, sizeof(private_key));
		return err;
	}

	spin_lock_bh(&melnode_dev.lock);
	melnode_dev.local_id = local_id;
	memcpy(melnode_dev.private_key, private_key, 32);
	memcpy(melnode_dev.public_key, public_key, 32);
	melnode_dev.listen_port = listen_port;
	melnode_dev.mtu = mtu;
	melnode_dev.fwmark = fwmark;
	melnode_cookie_checker_init(&melnode_dev.cookie_checker);
	melnode_cookie_checker_precompute_device_keys(&melnode_dev.cookie_checker, public_key);
	WRITE_ONCE(melnode_dev.configured, true);
	spin_unlock_bh(&melnode_dev.lock);
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

/* Tears down everything DEVICE_SET built: tuns, socket, keys. Shared
 * between DEVICE_DEL and melnode_exit() - the kernel does NOT pin a module
 * just because it has live net_devices registered (unlike, say, struct
 * file_operations.owner for a char device), so unloading this module
 * without first unregistering its tuns leaves live net_devices whose
 * net_device_ops function pointers point into now-freed module memory:
 * the next packet, or even just interface enumeration, touching one of
 * them jumps to unmapped memory and panics. Confirmed live: this is
 * exactly what happened testing on arke.
 */
static void melnode_teardown_device(void)
{
	LIST_HEAD(dead_tuns);
	int i;

	/* Lock order: rtnl outer, melnode_dev.lock inner - both
	 * unregister_netdevice_queue() and unregister_netdevice_many() must
	 * be called under rtnl_lock.
	 */
	rtnl_lock();
	spin_lock_bh(&melnode_dev.lock);
	for (i = 0; i < MELNODE_MAX_PEER; i++) {
		if (melnode_dev.tuns[i].valid)
			unregister_netdevice_queue(melnode_dev.tuns[i].dev, &dead_tuns);
	}
	memset(melnode_dev.tuns, 0, sizeof(melnode_dev.tuns));
	memset(melnode_dev.links, 0, sizeof(melnode_dev.links));
	memset(melnode_dev.routes, 0, sizeof(melnode_dev.routes));
	memzero_explicit(melnode_dev.private_key, sizeof(melnode_dev.private_key));
	memset(melnode_dev.public_key, 0, sizeof(melnode_dev.public_key));
	WRITE_ONCE(melnode_dev.configured, false);
	spin_unlock_bh(&melnode_dev.lock);
	unregister_netdevice_many(&dead_tuns);
	rtnl_unlock();

	/* Wait for any TUN_DESTROY-triggered async teardown (see
	 * melnode_queue_tun_teardown()) still in flight - its net_devices are
	 * already out of melnode_dev.tuns[] so the loop above never touched
	 * them, but melnode_tun_teardown_work_fn() is this module's own code
	 * and must not still be running (or queued) once this module unloads.
	 */
	flush_work(&melnode_dev.tun_teardown_work);

	melnode_socket_close(&melnode_dev);
}

static int melnode_nl_device_del(struct sk_buff *skb, struct genl_info *info)
{
	melnode_teardown_device();
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

	spin_lock_bh(&melnode_dev.lock);
	link = &melnode_dev.links[peer_id];

	if (link->valid && memcmp(link->public_key, pubkey, 32)) {
		spin_unlock_bh(&melnode_dev.lock);
		return -EEXIST;
	}

	if (!link->valid) {
		memcpy(link->public_key, pubkey, 32);
		melnode_noise_handshake_init(&link->handshake, pubkey);
		melnode_noise_precompute_static_static(&link->handshake, melnode_dev.private_key,
							pubkey);
		melnode_cookie_init(&link->cookie);
		melnode_cookie_precompute_peer_keys(&link->cookie, pubkey);
		link->valid = true;
	}

	if (info->attrs[MELNODE_A_ENDPOINT]) {
		int len = nla_len(info->attrs[MELNODE_A_ENDPOINT]);

		memcpy(link->endpoint, nla_data(info->attrs[MELNODE_A_ENDPOINT]), len);
		link->endpoint_len = len;
		link->has_endpoint = true;
	}
	/* else: leave whatever endpoint we already had, per API.md (a
	 * listen-only link learns its peer's address from traffic).
	 */

	spin_unlock_bh(&melnode_dev.lock);
	return 0;
}

static int melnode_nl_link_del(struct sk_buff *skb, struct genl_info *info)
{
	u8 peer_id;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	spin_lock_bh(&melnode_dev.lock);
	if (!melnode_dev.links[peer_id].valid) {
		spin_unlock_bh(&melnode_dev.lock);
		return -ENOENT;
	}
	memset(&melnode_dev.links[peer_id], 0, sizeof(melnode_dev.links[peer_id]));
	spin_unlock_bh(&melnode_dev.lock);
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
		    nla_put_u64_64bit(skb, MELNODE_A_LAST_HANDSHAKE,
				       melnode_link_last_handshake_ns(link), MELNODE_A_PAD) ||
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

	spin_lock_bh(&melnode_dev.lock);
	melnode_dev.routes[dst].valid = true;
	melnode_dev.routes[dst].nexthop = nexthop;
	spin_unlock_bh(&melnode_dev.lock);
	return 0;
}

static int melnode_nl_route_del(struct sk_buff *skb, struct genl_info *info)
{
	u8 dst;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_ROUTE_DST], &dst);
	if (err)
		return err;

	spin_lock_bh(&melnode_dev.lock);
	melnode_dev.routes[dst].valid = false;
	spin_unlock_bh(&melnode_dev.lock);
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
 * Handshake initiation - triggered from tun xmit (no valid session) or
 * INJECT-adjacent paths never; melnode has no timer-driven rekey yet (see
 * file header): a session is (re-)established lazily, on demand.
 * ------------------------------------------------------------------------ */

/* Caller holds no lock. Rate-limited per-link via
 * handshake->last_sent_initiation, mirroring WireGuard's REKEY_TIMEOUT.
 */
static void melnode_send_initiation(u8 peer_id)
{
	struct melnode_link *link;
	struct melnode_wire_handshake_initiation msg;
	u8 local_pub[32], local_priv[32];
	u64 now = ktime_get_coarse_boottime_ns();
	u8 endpoint[sizeof(struct sockaddr_in6)];
	int endpoint_len;
	bool have_endpoint;

	spin_lock_bh(&melnode_dev.lock);
	link = &melnode_dev.links[peer_id];
	if (!link->valid || !melnode_configured()) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}
	if ((s64)(link->handshake.last_sent_initiation + MELNODE_REKEY_TIMEOUT * NSEC_PER_SEC) >
	    (s64)now) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}
	link->handshake.last_sent_initiation = now;
	memcpy(local_pub, melnode_dev.public_key, 32);
	memcpy(local_priv, melnode_dev.private_key, 32);
	have_endpoint = link->has_endpoint;
	if (have_endpoint) {
		endpoint_len = link->endpoint_len;
		memcpy(endpoint, link->endpoint, endpoint_len);
	}

	if (!melnode_noise_handshake_create_initiation(&msg, &link->handshake, local_pub, local_priv,
							melnode_gen_index(peer_id))) {
		spin_unlock_bh(&melnode_dev.lock);
		memzero_explicit(local_priv, sizeof(local_priv));
		return;
	}
	melnode_cookie_add_mac_to_packet(&msg, sizeof(msg), &link->cookie);
	spin_unlock_bh(&melnode_dev.lock);
	memzero_explicit(local_priv, sizeof(local_priv));

	if (!have_endpoint)
		return; /* listen-only link with no known address yet */

	melnode_socket_send(&melnode_dev, &msg, sizeof(msg), endpoint, endpoint_len);
}

/* ------------------------------------------------------------------------
 * TUN netdevs: tun xmit is the "originate traffic to this peer" path.
 * ------------------------------------------------------------------------ */

static int melnode_tun_open(struct net_device *dev)
{
	return 0;
}

static int melnode_tun_stop(struct net_device *dev)
{
	return 0;
}

/* Encrypts and sends one melnode-header-prefixed plaintext over a link's
 * current sending keypair. Returns 0 on success. Caller holds no lock.
 */
static int melnode_send_data(u8 link_peer_id, const u8 *plain, size_t plain_len)
{
	struct melnode_link *link;
	struct melnode_keypair kp_copy;
	u8 endpoint[sizeof(struct sockaddr_in6)];
	int endpoint_len;
	struct melnode_wire_data *out;
	size_t out_len = sizeof(*out) + melnode_noise_encrypted_len(plain_len);
	u64 counter;
	int ret = 0;

	spin_lock_bh(&melnode_dev.lock);
	link = &melnode_dev.links[link_peer_id];
	if (!link->valid || !link->has_endpoint || !link->keypairs.current_kp.valid ||
	    !link->keypairs.current_kp.sending.is_valid) {
		spin_unlock_bh(&melnode_dev.lock);
		return -ENOENT;
	}
	kp_copy = link->keypairs.current_kp; /* snapshot key+index; counter is atomic */
	endpoint_len = link->endpoint_len;
	memcpy(endpoint, link->endpoint, endpoint_len);
	spin_unlock_bh(&melnode_dev.lock);

	out = kmalloc(out_len, GFP_ATOMIC);
	if (!out)
		return -ENOMEM;
	out->header.type = cpu_to_le32(MELNODE_WIRE_MSG_DATA);
	out->receiver_index = kp_copy.remote_index;
	/* Encrypt using the *live* counter (kp_copy.sending_counter is the
	 * same atomic64_t as the real keypair - atomics aren't struct-copy
	 * safe for this purpose, so re-fetch the live one).
	 */
	melnode_transport_encrypt(out->encrypted_data, plain, plain_len,
				   &melnode_dev.links[link_peer_id].keypairs.current_kp, &counter);
	out->counter = cpu_to_le64(counter);

	if (melnode_socket_send(&melnode_dev, out, out_len, endpoint, endpoint_len) < 0)
		ret = -EIO;
	else
		atomic64_add(plain_len, &link->tx_bytes);

	kfree(out);
	return ret;
}

static netdev_tx_t melnode_tun_xmit(struct sk_buff *skb, struct net_device *dev)
{
	struct melnode_tun_priv *priv = netdev_priv(dev);
	u8 dst_peer_id = priv->peer_id;
	u8 nexthop;
	bool have_route;
	u8 *plain;
	size_t plain_len;
	int err;

	if (skb_cow_head(skb, MELNODE_HDR_LEN)) {
		dev_kfree_skb(skb);
		DEV_STATS_INC(dev, tx_dropped);
		return NETDEV_TX_OK;
	}

	spin_lock_bh(&melnode_dev.lock);
	have_route = melnode_dev.routes[dst_peer_id].valid;
	nexthop = melnode_dev.routes[dst_peer_id].nexthop;
	spin_unlock_bh(&melnode_dev.lock);

	if (!have_route) {
		melnode_stat_inc(MELNODE_STAT_TX_NO_ROUTE);
		goto drop;
	}

	plain_len = MELNODE_HDR_LEN + skb->len;
	plain = kmalloc(plain_len, GFP_ATOMIC);
	if (!plain)
		goto drop;
	plain[MELNODE_HDR_OFF_PROTO] = 0;
	plain[MELNODE_HDR_OFF_SRC] = (u8)melnode_dev.local_id;
	plain[MELNODE_HDR_OFF_DST] = dst_peer_id;
	plain[MELNODE_HDR_OFF_TTL] = MELNODE_DEFAULT_TTL;
	if (skb_copy_bits(skb, 0, plain + MELNODE_HDR_LEN, skb->len)) {
		kfree(plain);
		goto drop;
	}

	err = melnode_send_data(nexthop, plain, plain_len);
	kfree(plain);
	if (err == -ENOENT) {
		melnode_send_initiation(nexthop);
		melnode_stat_inc(MELNODE_STAT_TX_QUEUE_FULL); /* no staging - see file header */
		goto drop;
	}
	if (err)
		goto drop;

	dev_kfree_skb(skb);
	DEV_STATS_INC(dev, tx_packets);
	return NETDEV_TX_OK;

drop:
	dev_kfree_skb(skb);
	DEV_STATS_INC(dev, tx_dropped);
	return NETDEV_TX_OK;
}

static const struct net_device_ops melnode_tun_netdev_ops = {
	.ndo_open = melnode_tun_open,
	.ndo_stop = melnode_tun_stop,
	.ndo_start_xmit = melnode_tun_xmit,
};

/* TUN_READQ_SIZE from drivers/net/tun.c - matched deliberately, not just for
 * cosmetic parity with `ip link` output. melnode-dp's tuns are real
 * /dev/net/tun devices, which get a genuine qdisc (fq_codel by default) with
 * this tx_queue_len; queued transmission there is deferred through
 * NET_TX_SOFTIRQ, which breaks up any synchronous call chain between chained
 * virtual devices. Our local-delivery path (netif_rx() onto node-<src>,
 * which the host's own IP stack may then forward straight back out a
 * *different* node-* tun for a downstream prefix - completely normal for
 * melnode's per-peer-tun design, see mnctl routes on any real node) is
 * exactly this kind of virtual-device chaining. IFF_NO_QUEUE (as used here
 * originally, matching WireGuard's own single-interface device.c) transmits
 * synchronously in the caller's context with no softirq boundary - safe for
 * WireGuard, whose xmit never re-enters another virtual device's RX path,
 * but for melnode this let a brief routing hairpin during convergence trip
 * __dev_queue_xmit()'s recursion guard immediately and repeatedly ("Dead
 * loop on virtual device", confirmed live on arke). A real qdisc absorbs
 * the same hairpin as ordinary queueing instead.
 */
#define MELNODE_TUN_TX_QUEUE_LEN 500

static void melnode_tun_setup(struct net_device *dev)
{
	dev->netdev_ops = &melnode_tun_netdev_ops;
	dev->type = ARPHRD_NONE;
	dev->flags = IFF_POINTOPOINT | IFF_NOARP | IFF_MULTICAST;
	dev->tx_queue_len = MELNODE_TUN_TX_QUEUE_LEN;
	dev->hard_header_len = 0;
	dev->addr_len = 0;
	dev->mtu = ETH_DATA_LEN;
	dev->min_mtu = 576;
	dev->max_mtu = 65000;
	dev->needs_free_netdev = true;
}

static int melnode_nl_tun_create(struct sk_buff *skb, struct genl_info *info)
{
	u8 peer_id;
	char name[IFNAMSIZ];
	struct melnode_tun *tun;
	struct net_device *dev;
	struct melnode_tun_priv *priv;
	struct sk_buff *reply;
	void *hdr;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;
	if (!info->attrs[MELNODE_A_TUN_NAME])
		return -EINVAL;
	nla_strscpy(name, info->attrs[MELNODE_A_TUN_NAME], sizeof(name));

	spin_lock_bh(&melnode_dev.lock);
	tun = &melnode_dev.tuns[peer_id];

	if (tun->valid) {
		bool same_name = !strcmp(tun->dev->name, name);

		spin_unlock_bh(&melnode_dev.lock);
		if (!same_name)
			return -EEXIST;
		goto reply_tun;
	}
	spin_unlock_bh(&melnode_dev.lock);

	/* Note: since TUN_DESTROY's actual unregister_netdevice() now happens
	 * asynchronously (melnode_queue_tun_teardown()), a DESTROY immediately
	 * followed by a CREATE reusing the same name can transiently race the
	 * old net_device's name still being held - register_netdevice() below
	 * returns -EEXIST in that case, same as any other genuine name clash.
	 * melnode-cp's reconcile loop (reconcile.go's EnsureTun) treats a
	 * TunCreate failure as transient and retries on its next pass, so this
	 * surfaces as a brief retry rather than a hard failure.
	 */
	rtnl_lock();
	dev = alloc_netdev(sizeof(*priv), name, NET_NAME_USER, melnode_tun_setup);
	if (!dev) {
		rtnl_unlock();
		return -ENOMEM;
	}
	dev_net_set(dev, &init_net);
	dev->mtu = melnode_dev.mtu;
	priv = netdev_priv(dev);
	priv->peer_id = peer_id;

	err = register_netdevice(dev);
	if (err) {
		free_netdev(dev);
		rtnl_unlock();
		return err == -EEXIST ? -EEXIST : err;
	}
	rtnl_unlock();

	spin_lock_bh(&melnode_dev.lock);
	tun = &melnode_dev.tuns[peer_id];
	if (tun->valid) {
		/* Lost a race with another TUN_CREATE for the same peer id;
		 * genl serializes doit calls for a given family by default,
		 * so this is defensive rather than expected.
		 */
		spin_unlock_bh(&melnode_dev.lock);
		rtnl_lock();
		unregister_netdevice(dev);
		rtnl_unlock();
		return -EEXIST;
	}
	tun->valid = true;
	tun->started = false;
	tun->dev = dev;
	spin_unlock_bh(&melnode_dev.lock);

reply_tun:
	reply = genlmsg_new(NLMSG_DEFAULT_SIZE, GFP_KERNEL);
	if (!reply)
		return -ENOMEM;
	hdr = genlmsg_put_reply(reply, info, &melnode_genl_family, 0, MELNODE_CMD_TUN_CREATE);
	if (!hdr)
		goto nla_put_failure;
	if (nla_put_string(reply, MELNODE_A_TUN_NAME, name) ||
	    nla_put_u8(reply, MELNODE_A_TUN_STARTED, tun->started))
		goto nla_put_failure;
	genlmsg_end(reply, hdr);
	return genlmsg_reply(reply, info);

nla_put_failure:
	nlmsg_free(reply);
	return -EMSGSIZE;
}

static int melnode_nl_tun_start(struct sk_buff *skb, struct genl_info *info)
{
	u8 peer_id;
	struct melnode_tun *tun;
	struct net_device *dev = NULL;
	int err = 0;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	spin_lock_bh(&melnode_dev.lock);
	tun = &melnode_dev.tuns[peer_id];
	if (!tun->valid) {
		spin_unlock_bh(&melnode_dev.lock);
		return -ENOENT;
	}
	if (!tun->started)
		dev = tun->dev;
	spin_unlock_bh(&melnode_dev.lock);

	if (dev) {
		rtnl_lock();
		err = dev_open(dev, NULL);
		rtnl_unlock();
		if (err)
			return err;

		spin_lock_bh(&melnode_dev.lock);
		melnode_dev.tuns[peer_id].started = true;
		spin_unlock_bh(&melnode_dev.lock);
	}
	return 0;
}

static int melnode_nl_tun_destroy(struct sk_buff *skb, struct genl_info *info)
{
	u8 peer_id;
	struct net_device *dev = NULL;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PEER_ID], &peer_id);
	if (err)
		return err;

	spin_lock_bh(&melnode_dev.lock);
	if (melnode_dev.tuns[peer_id].valid) {
		dev = melnode_dev.tuns[peer_id].dev;
		memset(&melnode_dev.tuns[peer_id], 0, sizeof(melnode_dev.tuns[peer_id]));
	}
	spin_unlock_bh(&melnode_dev.lock);

	if (dev)
		melnode_queue_tun_teardown(dev);
	return 0;
}

static int melnode_nl_tun_get(struct sk_buff *skb, struct netlink_callback *cb)
{
	int peer_id = cb->args[0];

	for (; peer_id < MELNODE_MAX_PEER; peer_id++) {
		struct melnode_tun *tun = &melnode_dev.tuns[peer_id];
		void *hdr;

		if (!tun->valid)
			continue;

		hdr = genlmsg_put(skb, NETLINK_CB(cb->skb).portid, cb->nlh->nlmsg_seq,
				   &melnode_genl_family, NLM_F_MULTI, MELNODE_CMD_TUN_GET);
		if (!hdr)
			break;
		if (nla_put_u32(skb, MELNODE_A_PEER_ID, peer_id) ||
		    nla_put_string(skb, MELNODE_A_TUN_NAME, tun->dev->name) ||
		    nla_put_u32(skb, MELNODE_A_MTU, tun->dev->mtu) ||
		    nla_put_u8(skb, MELNODE_A_TUN_STARTED, tun->started)) {
			genlmsg_cancel(skb, hdr);
			break;
		}
		genlmsg_end(skb, hdr);
	}

	cb->args[0] = peer_id;
	return skb->len;
}

/* ------------------------------------------------------------------------
 * INJECT - send a control packet directly out one link (no route lookup;
 * "one link" is literal per API.md), best-effort, no ACK.
 * ------------------------------------------------------------------------ */

static int melnode_nl_inject(struct sk_buff *skb, struct genl_info *info)
{
	u8 link_peer_id, dst, ttl, proto;
	const u8 *payload;
	size_t payload_len;
	u8 *plain;
	size_t plain_len;
	int err;

	err = melnode_get_u8_id(info->attrs[MELNODE_A_PKT_LINK], &link_peer_id);
	if (err)
		goto dropped;
	if (!info->attrs[MELNODE_A_PKT_PROTO] || !info->attrs[MELNODE_A_PKT_DST]) {
		err = -EINVAL;
		goto dropped;
	}
	proto = nla_get_u8(info->attrs[MELNODE_A_PKT_PROTO]);
	dst = nla_get_u8(info->attrs[MELNODE_A_PKT_DST]);
	ttl = info->attrs[MELNODE_A_PKT_TTL] ? nla_get_u8(info->attrs[MELNODE_A_PKT_TTL]) :
						MELNODE_DEFAULT_TTL;
	payload = info->attrs[MELNODE_A_PKT_DATA] ? nla_data(info->attrs[MELNODE_A_PKT_DATA]) : NULL;
	payload_len = info->attrs[MELNODE_A_PKT_DATA] ? nla_len(info->attrs[MELNODE_A_PKT_DATA]) : 0;

	if (payload_len > melnode_dev.mtu) {
		err = -EMSGSIZE;
		goto dropped;
	}

	plain_len = MELNODE_HDR_LEN + payload_len;
	plain = kmalloc(plain_len, GFP_KERNEL);
	if (!plain)
		goto dropped_noerr;
	plain[MELNODE_HDR_OFF_PROTO] = proto;
	plain[MELNODE_HDR_OFF_SRC] = (u8)melnode_dev.local_id;
	plain[MELNODE_HDR_OFF_DST] = dst;
	plain[MELNODE_HDR_OFF_TTL] = ttl;
	if (payload_len)
		memcpy(plain + MELNODE_HDR_LEN, payload, payload_len);

	err = melnode_send_data(link_peer_id, plain, plain_len);
	kfree(plain);
	if (err == -ENOENT)
		melnode_send_initiation(link_peer_id);
	if (err)
		goto dropped;

	melnode_stat_inc(MELNODE_STAT_INJECT_SENT);
	return 0;

dropped:
dropped_noerr:
	melnode_stat_inc(MELNODE_STAT_INJECT_DROPPED);
	return 0; /* INJECT has no reply/ACK per API.md - errors are silent */
}

/* ------------------------------------------------------------------------
 * Datagram receive dispatch - see melnode_socket.c for how this gets
 * called (rx_work, process context).
 * ------------------------------------------------------------------------ */

static void melnode_handle_handshake_initiation(u8 *data, size_t len, const void *from,
						 int from_len)
{
	struct melnode_wire_handshake_initiation *msg = (void *)data;
	u8 remote_static[32];
	u8 local_pub[32], local_priv[32];
	struct melnode_handshake_precursor pre;
	struct melnode_wire_handshake_response resp;
	enum melnode_cookie_mac_state mac_state;
	struct melnode_link *link = NULL;
	u8 peer_id;
	int i;

	if (len != sizeof(*msg))
		return;

	spin_lock_bh(&melnode_dev.lock);
	mac_state = melnode_cookie_validate_packet(&melnode_dev.cookie_checker, data, len, from,
						    from_len, true);
	spin_unlock_bh(&melnode_dev.lock);

	if (mac_state == MELNODE_COOKIE_INVALID_MAC)
		return;

	if (mac_state != MELNODE_COOKIE_VALID_MAC_WITH_COOKIE) {
		/* No valid cookie yet - tell the initiator to get one. This
		 * doesn't need to know which peer it is: the cookie reply's
		 * receiver_index just echoes the initiator's own
		 * sender_index back, and its encryption key comes from our
		 * device-wide cookie_encryption_key.
		 */
		struct melnode_wire_handshake_cookie reply;

		spin_lock_bh(&melnode_dev.lock);
		melnode_cookie_message_create(&reply, data, len, from, from_len, msg->sender_index,
					       &melnode_dev.cookie_checker);
		spin_unlock_bh(&melnode_dev.lock);
		melnode_socket_send(&melnode_dev, &reply, sizeof(reply), from, from_len);
		return;
	}

	spin_lock_bh(&melnode_dev.lock);
	if (!melnode_noise_handshake_consume_initiation1(msg, melnode_dev.public_key,
							  melnode_dev.private_key, remote_static,
							  &pre)) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}

	for (i = 0; i < MELNODE_MAX_PEER; i++) {
		if (melnode_dev.links[i].valid &&
		    !memcmp(melnode_dev.links[i].public_key, remote_static, 32)) {
			link = &melnode_dev.links[i];
			peer_id = i;
			break;
		}
	}
	if (!link) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}

	if (!melnode_noise_handshake_consume_initiation2(msg, &pre, &link->handshake)) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}

	/* Learn/roam the endpoint from any authenticated packet, per API.md
	 * ("a roamed listen-only link learns its peer's address from
	 * traffic").
	 */
	link->endpoint_len = from_len;
	memcpy(link->endpoint, from, from_len);
	link->has_endpoint = true;

	memcpy(local_pub, melnode_dev.public_key, 32);
	memcpy(local_priv, melnode_dev.private_key, 32);

	if (!melnode_noise_handshake_create_response(&resp, &link->handshake,
						      melnode_gen_index(peer_id))) {
		spin_unlock_bh(&melnode_dev.lock);
		memzero_explicit(local_priv, sizeof(local_priv));
		return;
	}
	melnode_cookie_add_mac_to_packet(&resp, sizeof(resp), &link->cookie);
	melnode_noise_handshake_begin_session(&link->handshake, &link->keypairs);
	link->last_handshake_unix_ns = ktime_get_real_ns();
	spin_unlock_bh(&melnode_dev.lock);
	memzero_explicit(local_priv, sizeof(local_priv));

	melnode_socket_send(&melnode_dev, &resp, sizeof(resp), from, from_len);
	melnode_send_event_handshake(peer_id, from, from_len);
}

static void melnode_handle_handshake_response(u8 *data, size_t len, const void *from, int from_len)
{
	struct melnode_wire_handshake_response *msg = (void *)data;
	struct melnode_link *link;
	u8 peer_id;
	u8 local_priv[32];
	bool ok;

	if (len != sizeof(*msg))
		return;

	peer_id = melnode_index_peer_id(msg->receiver_index);

	spin_lock_bh(&melnode_dev.lock);
	link = &melnode_dev.links[peer_id];
	if (!link->valid) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}

	/* mac1-only check is enough here: unlike an initiation (which
	 * arrives unsolicited from anyone), a response only makes sense in
	 * reply to an initiation *we* sent, so there's no amplification
	 * concern requiring mac2/cookie proof from the sender. mac1 is keyed
	 * by the *recipient's* static key - we're the recipient of this
	 * response, so that's our own device-wide cookie_checker, exactly as
	 * for an initiation.
	 */
	if (melnode_cookie_validate_packet(&melnode_dev.cookie_checker, data, len, from, from_len,
					    false) == MELNODE_COOKIE_INVALID_MAC) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}

	memcpy(local_priv, melnode_dev.private_key, 32);
	ok = melnode_noise_handshake_consume_response(msg, local_priv, &link->handshake);
	memzero_explicit(local_priv, sizeof(local_priv));
	if (!ok) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}

	link->endpoint_len = from_len;
	memcpy(link->endpoint, from, from_len);
	link->has_endpoint = true;

	melnode_noise_handshake_begin_session(&link->handshake, &link->keypairs);
	link->last_handshake_unix_ns = ktime_get_real_ns();
	spin_unlock_bh(&melnode_dev.lock);

	melnode_send_event_handshake(peer_id, from, from_len);
}

static void melnode_handle_cookie_reply(u8 *data, size_t len)
{
	struct melnode_wire_handshake_cookie *msg = (void *)data;
	u8 peer_id;

	if (len != sizeof(*msg))
		return;

	peer_id = melnode_index_peer_id(msg->receiver_index);

	spin_lock_bh(&melnode_dev.lock);
	if (melnode_dev.links[peer_id].valid)
		melnode_cookie_message_consume(msg, &melnode_dev.links[peer_id].cookie);
	spin_unlock_bh(&melnode_dev.lock);
}

/* Delivers a decrypted, melnode-header-prefixed plaintext that arrived over
 * link ingress_peer_id: PUNT (proto != 0), local tun delivery (dst == us),
 * or multi-hop re-forward (dst != us).
 */
static void melnode_route_plaintext(u8 ingress_peer_id, u8 *plain, size_t plain_len)
{
	u8 proto, src, dst, ttl;
	struct melnode_tun *tun;
	u8 tun_peer_id;
	bool have_route;
	u8 nexthop;

	if (plain_len < MELNODE_HDR_LEN) {
		melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
		return;
	}
	proto = plain[MELNODE_HDR_OFF_PROTO];
	src = plain[MELNODE_HDR_OFF_SRC];
	dst = plain[MELNODE_HDR_OFF_DST];
	ttl = plain[MELNODE_HDR_OFF_TTL];

	if (proto != 0) {
		melnode_send_punt(ingress_peer_id, proto, src, dst, ttl, plain + MELNODE_HDR_LEN,
				   plain_len - MELNODE_HDR_LEN);
		return;
	}

	if ((u32)dst == melnode_dev.local_id) {
		tun_peer_id = src; /* tun is keyed by the logical peer, not the physical link */
		spin_lock_bh(&melnode_dev.lock);
		tun = &melnode_dev.tuns[tun_peer_id];
		if (!tun->valid || !tun->started) {
			spin_unlock_bh(&melnode_dev.lock);
			melnode_stat_inc(MELNODE_STAT_RX_NO_TUN);
			return;
		}
		spin_unlock_bh(&melnode_dev.lock);
		{
			struct sk_buff *skb = netdev_alloc_skb(tun->dev, plain_len - MELNODE_HDR_LEN);

			if (!skb) {
				melnode_stat_inc(MELNODE_STAT_RX_QUEUE_FULL);
				return;
			}
			skb_put_data(skb, plain + MELNODE_HDR_LEN, plain_len - MELNODE_HDR_LEN);
			skb->protocol = (plain[MELNODE_HDR_LEN] >> 4) == 6 ? htons(ETH_P_IPV6) :
									      htons(ETH_P_IP);
			skb_reset_network_header(skb);
			skb->dev = tun->dev;
			netif_rx(skb);
		}
		return;
	}

	/* Multi-hop re-forward. */
	if (ttl <= 1) {
		melnode_stat_inc(MELNODE_STAT_RX_TTL_EXPIRED);
		return;
	}
	spin_lock_bh(&melnode_dev.lock);
	have_route = melnode_dev.routes[dst].valid;
	nexthop = melnode_dev.routes[dst].nexthop;
	spin_unlock_bh(&melnode_dev.lock);
	if (!have_route) {
		melnode_stat_inc(MELNODE_STAT_RX_NO_ROUTE);
		return;
	}
	plain[MELNODE_HDR_OFF_TTL] = ttl - 1;
	if (melnode_send_data(nexthop, plain, plain_len) == -ENOENT)
		melnode_send_initiation(nexthop);
}

static void melnode_handle_data(u8 *data, size_t len)
{
	struct melnode_wire_data *msg = (void *)data;
	size_t cipher_len;
	u8 peer_id;
	struct melnode_link *link;
	struct melnode_keypair *kp = NULL;
	bool is_next = false;
	u8 *plain;
	size_t plain_len;
	u64 counter;

	if (len < sizeof(*msg) + MELNODE_NOISE_AUTHTAG_LEN)
		return;
	cipher_len = len - sizeof(*msg);
	counter = le64_to_cpu(msg->counter);
	peer_id = melnode_index_peer_id(msg->receiver_index);

	spin_lock_bh(&melnode_dev.lock);
	link = &melnode_dev.links[peer_id];
	if (!link->valid) {
		spin_unlock_bh(&melnode_dev.lock);
		return;
	}
	/* msg->receiver_index is what the *sender* put in message_data.key_idx,
	 * which per WireGuard's own send-side convention is keypair->
	 * remote_index (the index the recipient - us - gave the sender during
	 * the handshake). So from our side, that's our own local_index, not
	 * remote_index - matching melnode_index_peer_id() above already
	 * treating it as one of our own indices.
	 */
	if (link->keypairs.current_kp.valid &&
	    link->keypairs.current_kp.local_index == msg->receiver_index) {
		kp = &link->keypairs.current_kp;
	} else if (link->keypairs.previous_kp.valid &&
		   link->keypairs.previous_kp.local_index == msg->receiver_index) {
		kp = &link->keypairs.previous_kp;
	} else if (link->keypairs.next_kp.valid &&
		   link->keypairs.next_kp.local_index == msg->receiver_index) {
		kp = &link->keypairs.next_kp;
		is_next = true;
	}
	if (!kp || !kp->receiving.is_valid) {
		spin_unlock_bh(&melnode_dev.lock);
		melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
		return;
	}
	/* Snapshot the receiving key; decrypt happens outside the lock. The
	 * replay counter itself is only ever touched under this lock (see
	 * below), so there's no lock-free race to worry about there.
	 */
	{
		u8 recv_key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];

		memcpy(recv_key, kp->receiving.key, sizeof(recv_key));
		spin_unlock_bh(&melnode_dev.lock);

		plain_len = cipher_len - MELNODE_NOISE_AUTHTAG_LEN;
		plain = kmalloc(cipher_len, GFP_KERNEL);
		if (!plain)
			return;
		if (!chacha20poly1305_decrypt(plain, msg->encrypted_data, cipher_len, NULL, 0,
					       counter, recv_key)) {
			kfree(plain);
			melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
			return;
		}
	}

	spin_lock_bh(&melnode_dev.lock);
	if (!melnode_replay_check(&kp->receiving_counter, counter)) {
		spin_unlock_bh(&melnode_dev.lock);
		kfree(plain);
		melnode_stat_inc(MELNODE_STAT_RX_BAD_PACKET);
		return;
	}
	if (is_next)
		melnode_noise_received_with_keypair(&link->keypairs, true);
	atomic64_add(plain_len, &link->rx_bytes);
	spin_unlock_bh(&melnode_dev.lock);

	if (plain_len > 0)
		melnode_route_plaintext(peer_id, plain, plain_len);
	kfree(plain);
}

void melnode_handle_datagram(u8 *data, size_t len, const void *from_addr, int from_len)
{
	__le32 type;

	if (!melnode_configured() || len < sizeof(struct melnode_wire_header))
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
	default:
		break;
	}
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
	{
		.cmd = MELNODE_CMD_TUN_CREATE,
		.doit = melnode_nl_tun_create,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_TUN_START,
		.doit = melnode_nl_tun_start,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_TUN_DESTROY,
		.doit = melnode_nl_tun_destroy,
		.flags = GENL_ADMIN_PERM,
	},
	{
		.cmd = MELNODE_CMD_TUN_GET,
		.dumpit = melnode_nl_tun_get,
		.flags = GENL_ADMIN_PERM,
	},
	/* X_QUIT: deliberately unregistered. "Userspace only" per API.md; a
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

	melnode_noise_init();
	melnode_socket_init_work(&melnode_dev);
	INIT_WORK(&melnode_dev.tun_teardown_work, melnode_tun_teardown_work_fn);
	err = melnode_ratelimiter_init();
	if (err)
		return err;

	err = genl_register_family(&melnode_genl_family);
	if (err) {
		melnode_ratelimiter_uninit();
		return err;
	}

	err = netlink_register_notifier(&melnode_netlink_notifier);
	if (err) {
		genl_unregister_family(&melnode_genl_family);
		melnode_ratelimiter_uninit();
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
	/* Idempotent (no-ops if DEVICE_DEL already ran) - needed here too, not
	 * just DEVICE_DEL: an admin can rmmod a configured device directly,
	 * and both a leaked bound UDP socket and (far worse - see
	 * melnode_teardown_device()'s comment) dangling tun net_devices
	 * would otherwise outlive the module with nothing left able to
	 * release them.
	 */
	melnode_teardown_device();
	melnode_ratelimiter_uninit();
	pr_info("melnode: unloaded\n");
}

module_init(melnode_init);
module_exit(melnode_exit);

MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("melnode kernel data plane (genl family \"melnode\")");
/* curve25519_generate_public()/chacha20poly1305_{en,de}crypt() live in
 * lib/crypto/{curve25519,chacha20poly1305}.c, each its own module on most
 * distro kernels (confirmed on the 7.2.7 test target). `modprobe melnode`
 * resolves these via the normal symbol-dependency path in modules.dep;
 * this is just so `modinfo melnode` documents it and depmod has an
 * explicit hint rather than only the implicit undefined-symbol one.
 * `insmod` does NOT resolve this - load both dependencies first if testing
 * with insmod directly.
 */
MODULE_SOFTDEP("pre: libcurve25519 libchacha20poly1305");
