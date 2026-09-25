/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_RATELIMITER_H
#define _MELNODE_RATELIMITER_H

#include <linux/types.h>

int melnode_ratelimiter_init(void);
void melnode_ratelimiter_uninit(void);
bool melnode_ratelimiter_allow(const void *from_addr, int from_len);

#endif
