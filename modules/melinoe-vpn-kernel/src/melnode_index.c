// SPDX-License-Identifier: GPL-2.0-only

#include <linux/hashtable.h>
#include <linux/random.h>
#include <linux/rcupdate.h>
#include <linux/spinlock.h>

#include "melnode_core.h"
#include "melnode_index.h"

static DEFINE_HASHTABLE(index_table, 10);
static DEFINE_SPINLOCK(index_lock);

static bool index_live(const struct melnode_link *link, __le32 index)
{
	const struct melnode_keypairs *kps = &link->keypairs;

	if (link->dead)
		return false;
	if (link->handshake.state != MELNODE_HANDSHAKE_ZEROED && link->handshake.local_index == index)
		return true;
	return (kps->current_kp.valid && kps->current_kp.local_index == index) ||
	       (kps->previous_kp.valid && kps->previous_kp.local_index == index) ||
	       (kps->next_kp.valid && kps->next_kp.local_index == index);
}

static bool index_used(__le32 index)
{
	struct melnode_index_slot *slot;

	hash_for_each_possible(index_table, slot, node, le32_to_cpu(index)) {
		if (slot->index == index)
			return true;
	}
	return false;
}

static void sync_locked(struct melnode_link *link)
{
	int i;

	for (i = 0; i < MELNODE_INDEX_SLOTS; i++) {
		struct melnode_index_slot *slot = &link->index_slots[i];

		if (slot->hashed && !index_live(link, slot->index)) {
			hash_del_rcu(&slot->node);
			slot->hashed = false;
		}
	}
}

void melnode_index_sync(struct melnode_link *link)
{
	spin_lock_bh(&index_lock);
	sync_locked(link);
	spin_unlock_bh(&index_lock);
}

u32 melnode_index_alloc(struct melnode_link *link)
{
	struct melnode_index_slot *slot = NULL;
	__le32 index;
	int i;

	spin_lock_bh(&index_lock);
	sync_locked(link);
	for (i = 0; i < MELNODE_INDEX_SLOTS; i++) {
		if (!link->index_slots[i].hashed) {
			slot = &link->index_slots[i];
			break;
		}
	}
	if (!slot) {
		spin_unlock_bh(&index_lock);
		return 0;
	}

	do {
		index = cpu_to_le32(get_random_u32());
	} while (!index || index_used(index));

	slot->index = index;
	slot->link = link;
	slot->hashed = true;
	hash_add_rcu(index_table, &slot->node, le32_to_cpu(index));
	spin_unlock_bh(&index_lock);
	return le32_to_cpu(index);
}

struct melnode_link *melnode_index_lookup(__le32 index)
{
	struct melnode_index_slot *slot;
	struct melnode_link *link = NULL;

	rcu_read_lock();
	hash_for_each_possible_rcu(index_table, slot, node, le32_to_cpu(index)) {
		if (slot->index == index) {
			link = slot->link;
			if (!kref_get_unless_zero(&link->kref))
				link = NULL;
			break;
		}
	}
	rcu_read_unlock();
	return link;
}
