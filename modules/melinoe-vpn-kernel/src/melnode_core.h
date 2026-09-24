/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * Shared device/link state, split out so melnode_socket.c (and future
 * files) can see struct melnode_device without pulling in all of
 * melnode_core.c's genl command handlers.
 */
#ifndef _MELNODE_CORE_H
#define _MELNODE_CORE_H

#include <linux/mutex.h>
#include <linux/workqueue.h>
#include <linux/skbuff.h>
#include <linux/in6.h>
#include <net/sock.h>

#include "melnode_genl.h"
#include "melnode_noise.h"
#include "melnode_cookie.h"

#define MELNODE_MAX_PEER 256 /* peer/node/dst ids are one byte on the wire */

struct melnode_link {
	bool valid;
	u8 public_key[32];
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
};

static inline u64 melnode_link_last_handshake_ns(const struct melnode_link *link)
{
	return link->last_handshake_unix_ns;
}

struct melnode_route {
	bool valid;
	u8 nexthop; /* peer id */
};

struct melnode_tun {
	bool valid;
	bool started;
	struct net_device *dev;
};

/* netdev_priv() for a melnode tun: enough to find our way back to the
 * device/link table from ndo_start_xmit.
 */
struct melnode_tun_priv {
	u8 peer_id;
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
	struct melnode_tun tuns[MELNODE_MAX_PEER];

	struct melnode_cookie_checker cookie_checker;

	struct socket *sock4;
	struct socket *sock6;
	void (*orig_sk_data_ready4)(struct sock *sk);
	void (*orig_sk_data_ready6)(struct sock *sk);
	struct work_struct rx_work;

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
