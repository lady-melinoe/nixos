// SPDX-License-Identifier: GPL-2.0-only

#include <linux/siphash.h>
#include <linux/slab.h>
#include <linux/mm.h>
#include <linux/mutex.h>
#include <linux/rcupdate.h>
#include <linux/workqueue.h>
#include <linux/in.h>
#include <linux/in6.h>

#include "melnode_ratelimiter.h"

static struct kmem_cache *entry_cache;
static hsiphash_key_t key;
static DEFINE_SPINLOCK(table_lock);
static DEFINE_MUTEX(init_lock);
static u64 init_refcnt;
static atomic_t total_entries = ATOMIC_INIT(0);
static unsigned int max_entries, table_size;
static void ratelimiter_gc_entries(struct work_struct *);
static DECLARE_DEFERRABLE_WORK(gc_work, ratelimiter_gc_entries);
static struct hlist_head *table_v4;
#if IS_ENABLED(CONFIG_IPV6)
static struct hlist_head *table_v6;
#endif

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
	atomic_dec(&total_entries);
}

static void entry_uninit(struct ratelimiter_entry *entry)
{
	hlist_del_rcu(&entry->hash);
	call_rcu(&entry->rcu, entry_free);
}

static void ratelimiter_gc_entries(struct work_struct *work)
{
	const u64 now = ktime_get_coarse_boottime_ns();
	struct ratelimiter_entry *entry;
	struct hlist_node *temp;
	unsigned int i;

	for (i = 0; i < table_size; ++i) {
		spin_lock(&table_lock);
		hlist_for_each_entry_safe(entry, temp, &table_v4[i], hash) {
			if (unlikely(!work) || now - entry->last_time_ns > NSEC_PER_SEC)
				entry_uninit(entry);
		}
#if IS_ENABLED(CONFIG_IPV6)
		hlist_for_each_entry_safe(entry, temp, &table_v6[i], hash) {
			if (unlikely(!work) || now - entry->last_time_ns > NSEC_PER_SEC)
				entry_uninit(entry);
		}
#endif
		spin_unlock(&table_lock);
		if (likely(work))
			cond_resched();
	}
	if (likely(work))
		queue_delayed_work(system_power_efficient_wq, &gc_work, HZ);
}

bool melnode_ratelimiter_allow(const void *from_addr, int from_len)
{
	const struct sockaddr *sa = from_addr;
	struct ratelimiter_entry *entry;
	struct hlist_head *bucket;
	u64 ip;

	if (sa->sa_family == AF_INET && from_len >= sizeof(struct sockaddr_in)) {
		const struct sockaddr_in *a4 = from_addr;

		ip = (u64 __force)a4->sin_addr.s_addr;
		bucket = &table_v4[hsiphash_1u32((u32)ip, &key) & (table_size - 1)];
	}
#if IS_ENABLED(CONFIG_IPV6)
	else if (sa->sa_family == AF_INET6 && from_len >= sizeof(struct sockaddr_in6)) {
		const struct sockaddr_in6 *a6 = from_addr;

		memcpy(&ip, &a6->sin6_addr, sizeof(ip));
		bucket = &table_v6[hsiphash_2u32((u32)(ip >> 32), (u32)ip, &key) & (table_size - 1)];
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

	if (atomic_inc_return(&total_entries) > max_entries)
		goto err_oom;

	entry = kmem_cache_alloc(entry_cache, GFP_KERNEL);
	if (unlikely(!entry))
		goto err_oom;

	entry->ip = ip;
	INIT_HLIST_NODE(&entry->hash);
	spin_lock_init(&entry->lock);
	entry->last_time_ns = ktime_get_coarse_boottime_ns();
	entry->tokens = TOKEN_MAX - PACKET_COST;
	spin_lock(&table_lock);
	hlist_add_head_rcu(&entry->hash, bucket);
	spin_unlock(&table_lock);
	return true;

err_oom:
	atomic_dec(&total_entries);
	return false;
}

int melnode_ratelimiter_init(void)
{
	mutex_lock(&init_lock);
	if (++init_refcnt != 1)
		goto out;

	entry_cache = KMEM_CACHE(ratelimiter_entry, 0);
	if (!entry_cache)
		goto err;

	table_size = (totalram_pages() > (1U << 30) / PAGE_SIZE) ?
			     8192 :
			     max_t(unsigned long, 16,
				   roundup_pow_of_two((totalram_pages() << PAGE_SHIFT) /
						      (1U << 14) / sizeof(struct hlist_head)));
	max_entries = table_size * 8;

	table_v4 = kvcalloc(table_size, sizeof(*table_v4), GFP_KERNEL);
	if (unlikely(!table_v4))
		goto err_kmemcache;

#if IS_ENABLED(CONFIG_IPV6)
	table_v6 = kvcalloc(table_size, sizeof(*table_v6), GFP_KERNEL);
	if (unlikely(!table_v6)) {
		kvfree(table_v4);
		goto err_kmemcache;
	}
#endif

	get_random_bytes(&key, sizeof(key));
	queue_delayed_work(system_power_efficient_wq, &gc_work, HZ);
out:
	mutex_unlock(&init_lock);
	return 0;

err_kmemcache:
	kmem_cache_destroy(entry_cache);
err:
	--init_refcnt;
	mutex_unlock(&init_lock);
	return -ENOMEM;
}

void melnode_ratelimiter_uninit(void)
{
	mutex_lock(&init_lock);
	if (!init_refcnt || --init_refcnt)
		goto out;

	cancel_delayed_work_sync(&gc_work);
	ratelimiter_gc_entries(NULL);
	rcu_barrier();
	kvfree(table_v4);
#if IS_ENABLED(CONFIG_IPV6)
	kvfree(table_v6);
#endif
	kmem_cache_destroy(entry_cache);
out:
	mutex_unlock(&init_lock);
}
