// SPDX-License-Identifier: GPL-2.0-only

#include <linux/siphash.h>
#include <linux/slab.h>
#include <linux/mm.h>
#include <linux/rcupdate.h>
#include <linux/workqueue.h>
#include <linux/in.h>
#include <linux/in6.h>

#include "melnode_ratelimiter.h"

static struct kmem_cache *entry_cache;

struct ratelimiter_entry {
	u64 last_time_ns, tokens, ip;
	spinlock_t lock;
	struct hlist_node hash;
	struct rcu_head rcu;
};

enum {
	PACKETS_PER_SECOND = 20,
	PACKETS_BURSTABLE = 5,
	PACKET_COST = NSEC_PER_SEC / PACKETS_PER_SECOND,
	TOKEN_MAX = PACKET_COST * PACKETS_BURSTABLE,
};

static void entry_free(struct rcu_head *rcu)
{
	kmem_cache_free(entry_cache, container_of(rcu, struct ratelimiter_entry, rcu));
}

static void entry_uninit(struct melnode_ratelimiter *rl, struct ratelimiter_entry *entry)
{
	hlist_del_rcu(&entry->hash);
	atomic_dec(&rl->total_entries);
	call_rcu(&entry->rcu, entry_free);
}

static void gc_entries(struct melnode_ratelimiter *rl, bool all)
{
	const u64 now = ktime_get_coarse_boottime_ns();
	struct ratelimiter_entry *entry;
	struct hlist_node *temp;
	unsigned int i;

	for (i = 0; i < rl->table_size; ++i) {
		spin_lock(&rl->table_lock);
		hlist_for_each_entry_safe(entry, temp, &rl->table_v4[i], hash) {
			if (all || now - entry->last_time_ns > NSEC_PER_SEC)
				entry_uninit(rl, entry);
		}
#if IS_ENABLED(CONFIG_IPV6)
		hlist_for_each_entry_safe(entry, temp, &rl->table_v6[i], hash) {
			if (all || now - entry->last_time_ns > NSEC_PER_SEC)
				entry_uninit(rl, entry);
		}
#endif
		spin_unlock(&rl->table_lock);
		if (!all)
			cond_resched();
	}
}

static void gc_work_fn(struct work_struct *work)
{
	struct melnode_ratelimiter *rl =
		container_of(to_delayed_work(work), struct melnode_ratelimiter, gc_work);

	gc_entries(rl, false);
	queue_delayed_work(system_power_efficient_wq, &rl->gc_work, HZ);
}

bool melnode_ratelimiter_allow(struct melnode_ratelimiter *rl, const void *from_addr, int from_len)
{
	const struct sockaddr *sa = from_addr;
	struct ratelimiter_entry *entry;
	struct hlist_head *bucket;
	u64 ip;

	if (sa->sa_family == AF_INET && from_len >= sizeof(struct sockaddr_in)) {
		const struct sockaddr_in *a4 = from_addr;

		ip = (u64 __force)a4->sin_addr.s_addr;
		bucket = &rl->table_v4[hsiphash_1u32((u32)ip, &rl->key) & (rl->table_size - 1)];
	}
#if IS_ENABLED(CONFIG_IPV6)
	else if (sa->sa_family == AF_INET6 && from_len >= sizeof(struct sockaddr_in6)) {
		const struct sockaddr_in6 *a6 = from_addr;

		memcpy(&ip, &a6->sin6_addr, sizeof(ip));
		bucket = &rl->table_v6[hsiphash_2u32((u32)(ip >> 32), (u32)ip, &rl->key) &
				(rl->table_size - 1)];
	}
#endif
	else
		return false;

	rcu_read_lock();
	hlist_for_each_entry_rcu(entry, bucket, hash) {
		if (entry->ip == ip) {
			u64 now, tokens;
			bool ret;

			spin_lock(&entry->lock);
			now = ktime_get_coarse_boottime_ns();
			tokens = min_t(u64, TOKEN_MAX, entry->tokens + now - entry->last_time_ns);
			entry->last_time_ns = now;
			ret = tokens >= PACKET_COST;
			entry->tokens = ret ? tokens - PACKET_COST : tokens;
			spin_unlock(&entry->lock);
			rcu_read_unlock();
			return ret;
		}
	}
	rcu_read_unlock();

	if (atomic_inc_return(&rl->total_entries) > rl->max_entries)
		goto err_oom;

	entry = kmem_cache_alloc(entry_cache, GFP_KERNEL);
	if (unlikely(!entry))
		goto err_oom;

	entry->ip = ip;
	INIT_HLIST_NODE(&entry->hash);
	spin_lock_init(&entry->lock);
	entry->last_time_ns = ktime_get_coarse_boottime_ns();
	entry->tokens = TOKEN_MAX - PACKET_COST;
	spin_lock(&rl->table_lock);
	hlist_add_head_rcu(&entry->hash, bucket);
	spin_unlock(&rl->table_lock);
	return true;

err_oom:
	atomic_dec(&rl->total_entries);
	return false;
}

int melnode_ratelimiter_module_init(void)
{
	entry_cache = KMEM_CACHE(ratelimiter_entry, 0);
	return entry_cache ? 0 : -ENOMEM;
}

void melnode_ratelimiter_module_exit(void)
{
	rcu_barrier();
	kmem_cache_destroy(entry_cache);
}

int melnode_ratelimiter_init(struct melnode_ratelimiter *rl)
{
	spin_lock_init(&rl->table_lock);
	atomic_set(&rl->total_entries, 0);
	INIT_DEFERRABLE_WORK(&rl->gc_work, gc_work_fn);

	rl->table_size = (totalram_pages() > (1U << 30) / PAGE_SIZE) ?
				 8192 :
				 max_t(unsigned long, 16,
				       roundup_pow_of_two((totalram_pages() << PAGE_SHIFT) /
							  (1U << 14) / sizeof(struct hlist_head)));
	rl->max_entries = rl->table_size * 8;

	rl->table_v4 = kvcalloc(rl->table_size, sizeof(*rl->table_v4), GFP_KERNEL);
	if (unlikely(!rl->table_v4))
		return -ENOMEM;

#if IS_ENABLED(CONFIG_IPV6)
	rl->table_v6 = kvcalloc(rl->table_size, sizeof(*rl->table_v6), GFP_KERNEL);
	if (unlikely(!rl->table_v6)) {
		kvfree(rl->table_v4);
		return -ENOMEM;
	}
#endif

	get_random_bytes(&rl->key, sizeof(rl->key));
	queue_delayed_work(system_power_efficient_wq, &rl->gc_work, HZ);
	return 0;
}

void melnode_ratelimiter_uninit(struct melnode_ratelimiter *rl)
{
	cancel_delayed_work_sync(&rl->gc_work);
	gc_entries(rl, true);
	rcu_barrier();
	kvfree(rl->table_v4);
#if IS_ENABLED(CONFIG_IPV6)
	kvfree(rl->table_v6);
#endif
}
