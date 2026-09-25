/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_CORE_H
#define _MELNODE_CORE_H

#include <linux/mutex.h>
#include <linux/spinlock.h>
#include <linux/rwsem.h>
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

#define MELNODE_MAX_PEER 256
#define MELNODE_HANDSHAKE_DSCP 0x88
#define MELNODE_MIN_MTU 576
#define MELNODE_MAX_MTU 65000
#define MELNODE_LINK_OUTQ_SIZE 1024

struct melnode_endpoint {
	u8 addr[sizeof(struct sockaddr_in6)];
	u8 addr_len;
	bool has_src;
	int src_ifindex;
	union {
		__be32 v4;
		struct in6_addr v6;
	} src;
};

#define MELNODE_INDEX_SLOTS 5

struct melnode_index_slot {
	struct hlist_node node;
	struct melnode_link *link;
	__le32 index;
	bool hashed;
};

struct melnode_hs_item {
	struct list_head list;
	struct melnode_endpoint from;
	size_t len;
	u8 data[];
};

struct melnode_link {
	struct kref kref;
	struct rcu_head rcu;
	u8 peer_id;
	u8 public_key[32];

	spinlock_t lock;
	bool has_endpoint;
	bool clear_src;
	struct melnode_endpoint endpoint;
	atomic64_t tx_bytes;
	atomic64_t rx_bytes;
	u64 last_handshake_unix_ns;

	struct melnode_handshake handshake;
	struct melnode_keypairs keypairs;
	struct melnode_cookie cookie;

	bool dead;
	u64 keepalive_at;
	u64 new_handshake_at;
	u64 retry_at;
	u64 wipe_at;
	u32 handshake_attempts;
	bool sent_last_minute_handshake;
	bool need_another_keepalive;
	struct melnode_index_slot index_slots[MELNODE_INDEX_SLOTS];
	struct delayed_work timer;

	struct ptr_ring outq;
	struct work_struct outq_work;
	struct list_head teardown_node;
};

struct melnode_route {
	struct rcu_head rcu;
	u8 nexthop;
};

enum melnode_tun_state {
	MELNODE_TUN_CREATED,
	MELNODE_TUN_STARTED,
	MELNODE_TUN_DESTROYING,
};

struct melnode_tun {
	struct rcu_head rcu;
	u8 peer_id;
	struct net_device *dev;
	enum melnode_tun_state state;
};

struct melnode_tun_priv {
	u8 peer_id;
	struct gro_cells gcells;
};

struct melnode_device {
	struct mutex config_lock;
	bool configured;

	u32 local_id;
	u16 listen_port;
	u32 mtu;
	u32 fwmark;

	spinlock_t key_lock;
	u8 private_key[32];
	u8 public_key[32];

	spinlock_t attach_lock;
	u32 attached_portid;

	struct mutex tables_mutex;
	struct melnode_route __rcu *routes[MELNODE_MAX_PEER];
	struct melnode_link __rcu *links[MELNODE_MAX_PEER];
	struct melnode_tun __rcu *tuns[MELNODE_MAX_PEER];

	struct melnode_cookie_checker cookie_checker;

	spinlock_t hs_lock;
	struct list_head hs_queue;
	unsigned int hs_count;
	struct work_struct hs_work;
	u64 last_under_load;

	struct rw_semaphore sock_sem;
	struct socket *sock4;
	struct socket *sock6;
	void (*orig_sk_data_ready4)(struct sock *sk);
	void (*orig_sk_data_ready6)(struct sock *sk);
	struct work_struct rx_work;
	/* rx_work's receive buffer: allocated with the sockets, freed once
	 * rx_work can no longer run (melnode_socket_open/close). rx_work never
	 * runs concurrently with itself, so one is enough.
	 */
	u8 *rx_buf;

	spinlock_t pending_teardown_lock;
	struct list_head pending_teardown;
	struct work_struct tun_teardown_work;

	atomic64_t stats[MELNODE_STAT_RX_QUEUE_FULL + 1];
};

extern struct melnode_device melnode_dev;

/* The module's own workqueue (receive, handshakes, link send queues, tun
 * teardown): keeps the data path off system_wq, and destroy_workqueue() at
 * unload waits for every item still queued on it.
 */
extern struct workqueue_struct *melnode_wq;

void melnode_stat_inc(enum melnode_stat id);
void melnode_get_keys(u8 public_key[32], u8 private_key[32]);
void melnode_handle_datagram(u8 *data, size_t len, const struct melnode_endpoint *from);

#endif /* _MELNODE_CORE_H */
