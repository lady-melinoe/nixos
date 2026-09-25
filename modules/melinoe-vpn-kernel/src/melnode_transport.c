// SPDX-License-Identifier: GPL-2.0-only

#include <linux/bitops.h>
#include <linux/log2.h>
#include <crypto/chacha20poly1305.h>

#include "melnode_transport.h"

#define COUNTER_WORDS (MELNODE_COUNTER_BITS_TOTAL / BITS_PER_LONG)

bool melnode_replay_check(struct melnode_replay_counter *counter, u64 their_counter)
{
	unsigned long index, index_current, top, i;

	if (unlikely(counter->counter >= MELNODE_REJECT_AFTER_MESSAGES + 1 ||
		     their_counter >= MELNODE_REJECT_AFTER_MESSAGES))
		return false;

	++their_counter;

	if (unlikely((MELNODE_COUNTER_WINDOW_SIZE + their_counter) < counter->counter))
		return false;

	index = their_counter >> ilog2(BITS_PER_LONG);

	if (likely(their_counter > counter->counter)) {
		index_current = counter->counter >> ilog2(BITS_PER_LONG);
		top = min_t(unsigned long, index - index_current, COUNTER_WORDS);
		for (i = 1; i <= top; ++i)
			counter->backtrack[(i + index_current) & (COUNTER_WORDS - 1)] = 0;
		counter->counter = their_counter;
	}

	index &= COUNTER_WORDS - 1;
	return !test_and_set_bit(their_counter & (BITS_PER_LONG - 1), &counter->backtrack[index]);
}

void melnode_transport_encrypt(u8 *dst, const u8 *plain, size_t plain_len,
			       const u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN], u64 counter)
{
	chacha20poly1305_encrypt(dst, plain, plain_len, NULL, 0, counter, key);
}
