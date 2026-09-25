/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * Shared device/link state, split out so melnode_socket.c (and future
 * files) can see struct melnode_device without pulling in all of
 * melnode_core.c's genl command handlers.
 *
 * Locking/lifetime model, most-to-least hot-path:
 *  - routes[]/links[]/tuns[] are RCU-published arrays of pointers, one
 *    slot per peer id (0..255 IS the index - no separate lookup
 *    structure). Readers: rcu_read_lock()/rcu_dereference()/
 *    rcu_read_unlock(), no blocking, no lock. Writers (genl doit only):
 *    tables_mutex + rcu_assign_pointer()/kfree_rcu() per slot - never a
 *    whole-table copy-and-swap.
 *  - A link struct is refcounted (kref): a reader that finds one via RCU
 *    must kref_get_unless_zero() before using it outside the RCU read
 *    section (e.g. stashing it in a queued item), and kref_put() when
 *    done. The release callback frees via kfree_rcu(), not kfree().
 *  - A tun's net_device lifetime is NOT kref'd here - it leans on the
 *    kernel's own dev_hold()/dev_put() plus gro_cells' .ndo_uninit-
 *    synchronized teardown (see melnode_tun_priv below). No bespoke
 *    refcounting for tuns.
 *  - A link's own mutable crypto/session state (handshake, keypairs,
 *    cookie, endpoint) is protected by that link's own spinlock, not a
 *    device-wide one. Never held across crypto, socket, or tun I/O -
 *    snapshot the needed fields, unlock, then do the I/O.
 *  - cookie_checker's secret rotation is protected by its own
 *    secret_lock (melnode_cookie.h), independent of everything else
 *    here.
 *  - Lock order: rtnl_lock (tun register/unregister only) -> tables_mutex
 *    -> a link's own spinlock. cookie_checker's secret_lock and
 *    config_lock are both independent of that chain.
 */
#ifndef _MELNODE_CORE_H
#define _MELNODE_CORE_H

#include <linux/mutex.h>
#include <linux/spinlock.h>
#include <linux/rcupdate.h>
#include <linux/kref.h>
#include <linux/ptr_ring.h>
#include <linux/list.h>
#include <linux/workqueue.h>
#include <linux/skbuff.h>
#include <linux/in6.h>
#include <net/sock.h>
#include <net/gro_cells.h>

#include "melnode_genl.h"
#include "melnode_noise.h"
#include "melnode_cookie.h"

#define MELNODE_MAX_PEER 256 /* peer/node/dst ids are one byte on the wire */

/* Forward-onward datapath queue depth per link (encrypt-and-send to the
 * next hop). Matches melnode-dp's own tunWriterQueueSize (router.go),
 * arrived at there after measurement showing an earlier, much smaller
 * value became the throughput bottleneck - not guessed independently
 * here.
 */
#define MELNODE_LINK_OUTQ_SIZE 1024

/* One peer's Noise session + routing-queue egress. RCU-published via
 * melnode_device.links[], refcounted (kref) - see this file's header
 * comment. `peer_id` and `public_key` are the RCU-published identity:
 * written once before publish, read lock-free by the handshake-initiation
 * pubkey scan, never mutated after. Everything from `lock` down is
 * mutable and protected by that spinlock.
 */
struct melnode_link {
	struct kref kref;
	struct rcu_head rcu;
	u8 peer_id;
	u8 public_key[32];

	spinlock_t lock;
	bool has_endpoint;
	u8 endpoint[sizeof(struct sockaddr_in6)];
	u8 endpoint_len;
	atomic64_t tx_bytes;
	atomic64_t rx_bytes;
	/* Wall-clock (ktime_get_real_ns()) timestamp of the last completed
	 * handshake, for MELNODE_A_LAST_HANDSHAKE ("unix ns" per API.md) - a
	 * separate field from the keypairs' own birthdate, which is
	 * boottime-relative (ktime_get_coarse_boottime_ns(), matching
	 * WireGuard's noise.c) and used for rekey/expiry math, not reporting.
	 * Set in melnode_core.c right after a successful begin_session().
	 */
	u64 last_handshake_unix_ns;

	struct melnode_handshake handshake;
	struct melnode_keypairs keypairs;
	struct melnode_cookie cookie;

	/* Forward-onward (encrypt + send to this link) queue - see
	 * melnode_routing.c. Each queued item carries its own kref on this
	 * link, released by the consumer after processing it (see this
	 * file's header comment) - not released by whoever enqueued it.
	 */
	struct ptr_ring outq;
	struct work_struct outq_work;
};

static inline u64 melnode_link_last_handshake_ns(const struct melnode_link *link)
{
	return link->last_handshake_unix_ns;
}

/* RCU-published, no lifetime concerns beyond RCU/kfree_rcu: pure data, no
 * embedded kernel object, no per-item reference needed (a route lookup
 * just copies the nexthop byte out before the RCU read section ends).
 */
struct melnode_route {
	struct rcu_head rcu;
	u8 nexthop; /* peer id */
};

enum melnode_tun_state {
	MELNODE_TUN_CREATED,
	MELNODE_TUN_STARTED,
	MELNODE_TUN_DESTROYING,
};

/* RCU-published via melnode_device.tuns[]. Not kref'd - see this file's
 * header comment: lifetime is dev_hold()/dev_put() plus gro_cells' own
 * .ndo_uninit-synchronized teardown. `state` is a single scalar mutated
 * rarely (TUN_START, once) and read on every locally-destined packet:
 * WRITE_ONCE() under tables_mutex, READ_ONCE() on the reader side - not a
 * reason to replace the whole entry.
 */
struct melnode_tun {
	struct rcu_head rcu;
	u8 peer_id;
	struct net_device *dev;
	enum melnode_tun_state state;
};

/* netdev_priv() for a melnode tun: enough to find our way back to the
 * device/link table from ndo_start_xmit, plus the per-tun GRO cells used
 * for local delivery (melnode_routing.c), the same mechanism
 * vxlan/geneve/gtp use to hand a decapsulated packet back into the stack
 * without deep recursion. gro_cells_init() at TUN_CREATE,
 * gro_cells_destroy() from .ndo_uninit at teardown.
 */
struct melnode_tun_priv {
	u8 peer_id;
	struct gro_cells gcells;
};

struct melnode_device {
	/* Device identity/listen port/mtu/fwmark - touched only from genl
	 * doit (DEVICE_SET/DEVICE_DEL), which already sleeps (socket open,
	 * GFP_KERNEL), so this is a mutex, not a spinlock. Independent of
	 * every other lock in this struct - never taken alongside them.
	 */
	struct mutex config_lock;
	bool configured;

	u32 local_id;
	u8 private_key[32];
	u8 public_key[32];
	u16 listen_port;
	u32 mtu;
	u32 fwmark;

	/* Small dedicated lock, not part of the hot path: ATTACH (genl doit)
	 * and the netlink release notifier (melnode_netlink_notifier_call,
	 * arbitrary context) both need an atomic check-and-clear, not just
	 * independent WRITE_ONCE/READ_ONCE, or a release notification for an
	 * old portid can race a fresh ATTACH and incorrectly clear it.
	 */
	spinlock_t attach_lock;
	u32 attached_portid; /* 0 = nobody attached; protected by attach_lock */

	/* Control-path writer serialization for routes[]/links[]/tuns[] -
	 * see this file's header comment. Readers never take this; they use
	 * RCU instead.
	 */
	struct mutex tables_mutex;
	struct melnode_route __rcu *routes[MELNODE_MAX_PEER];
	struct melnode_link __rcu *links[MELNODE_MAX_PEER];
	struct melnode_tun __rcu *tuns[MELNODE_MAX_PEER];

	struct melnode_cookie_checker cookie_checker;

	struct socket *sock4;
	struct socket *sock6;
	void (*orig_sk_data_ready4)(struct sock *sk);
	void (*orig_sk_data_ready6)(struct sock *sk);
	struct work_struct rx_work;

	/* TUN_DESTROY hands the actual unregister_netdevice() off here instead
	 * of doing it synchronously in the genl doit call. unregister_netdevice()
	 * can stall for a long time (the classic "waiting for dev to become
	 * free" case, e.g. lingering route/neighbour references, and now also
	 * genuinely waiting out any outstanding dev_hold() from an in-flight
	 * RCU reader - see this file's header comment) while holding
	 * rtnl_lock - since genl delivery to a kernel family runs the doit
	 * handler synchronously inside the sender's write(2), a stall there
	 * blocks melnode-cp's entire control channel (every other command, and
	 * transitively peer liveness) for as long as it lasts. See
	 * melnode_tun_teardown_work_fn() in melnode_core.c.
	 */
	spinlock_t pending_teardown_lock;
	struct list_head pending_teardown;
	struct work_struct tun_teardown_work;

	atomic64_t stats[MELNODE_STAT_RX_QUEUE_FULL + 1];
};

extern struct melnode_device melnode_dev;

void melnode_stat_inc(enum melnode_stat id);

/* Called by melnode_socket.c's rx_work for each received datagram - all of
 * melnode's wire-format dispatch (handshake/cookie/data messages) lives in
 * melnode_core.c.
 */
void melnode_handle_datagram(u8 *data, size_t len, const void *from_addr, int from_len);

#endif /* _MELNODE_CORE_H */
