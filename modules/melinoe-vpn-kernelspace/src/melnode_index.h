/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_INDEX_H
#define _MELNODE_INDEX_H

#include <linux/types.h>

struct melnode_link;

u32 melnode_index_alloc(struct melnode_link *link);
void melnode_index_sync(struct melnode_link *link);
struct melnode_link *melnode_index_lookup(__le32 index);

#endif
