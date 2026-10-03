/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_RATELIMITER_H
#define _MELNODE_RATELIMITER_H

#include <linux/types.h>
#include <linux/siphash.h>
#include <linux/spinlock.h>
#include <linux/workqueue.h>
#include <linux/list.h>

struct melnode_ratelimiter {
	hsiphash_key_t key;
	spinlock_t table_lock;
	atomic_t total_entries;
	unsigned int max_entries;
	unsigned int table_size;
	struct delayed_work gc_work;
	struct hlist_head *table_v4;
#if IS_ENABLED(CONFIG_IPV6)
	struct hlist_head *table_v6;
#endif
};

int melnode_ratelimiter_module_init(void);
void melnode_ratelimiter_module_exit(void);

int melnode_ratelimiter_init(struct melnode_ratelimiter *rl);
void melnode_ratelimiter_uninit(struct melnode_ratelimiter *rl);
bool melnode_ratelimiter_allow(struct melnode_ratelimiter *rl, const void *from_addr, int from_len);

#endif
