/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_TRANSPORT_H
#define _MELNODE_TRANSPORT_H

#include <linux/types.h>

#include "melnode_noise.h"

bool melnode_replay_check(struct melnode_replay_counter *counter, u64 their_counter);

void melnode_transport_encrypt(u8 *dst, const u8 *plain, size_t plain_len,
			       const u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN], u64 counter);

#endif /* _MELNODE_TRANSPORT_H */
