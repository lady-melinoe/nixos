/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_INDEX_H
#define _MELNODE_INDEX_H

#include <linux/types.h>
#include <linux/hashtable.h>
#include <linux/spinlock.h>

struct melnode_link;
struct melnode_instance;

struct melnode_index_table {
	DECLARE_HASHTABLE(table, 10);
	spinlock_t lock;
};

void melnode_index_table_init(struct melnode_index_table *t);
u32 melnode_index_alloc(struct melnode_link *link);
void melnode_index_sync(struct melnode_link *link);
struct melnode_link *melnode_index_lookup(struct melnode_instance *inst, __le32 index);

#endif
