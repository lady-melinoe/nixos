// SPDX-License-Identifier: GPL-2.0-only
/*
 * Data-message encrypt/decrypt and replay protection - see
 * melnode_transport.h for what's ported verbatim (the replay window) versus
 * simplified (linear-buffer crypto instead of scatterlists).
 */

#include <linux/atomic.h>
#include <linux/bitops.h>
#include <linux/log2.h>
#include <crypto/chacha20poly1305.h>

#include "melnode_transport.h"

/* RFC 6479, ported from wg-receive.c's counter_validate(). */
bool melnode_replay_check(struct melnode_replay_counter *counter, u64 their_counter)
{
	unsigned long index, index_current, top, i;
	bool ret = false;

	if (unlikely(counter->counter >= MELNODE_REJECT_AFTER_MESSAGES + 1 ||
		     their_counter >= MELNODE_REJECT_AFTER_MESSAGES))
		return false;

	++their_counter;

	if (unlikely((MELNODE_COUNTER_WINDOW_SIZE + their_counter) < counter->counter))
		return false;

	index = their_counter >> ilog2(BITS_PER_LONG);

	if (likely(their_counter > counter->counter)) {
		index_current = counter->counter >> ilog2(BITS_PER_LONG);
		top = min_t(unsigned long, index - index_current,
			    MELNODE_COUNTER_BITS_TOTAL / BITS_PER_LONG);
		for (i = 1; i <= top; ++i)
			counter->backtrack[(i + index_current) &
					    ((MELNODE_COUNTER_BITS_TOTAL / BITS_PER_LONG) - 1)] = 0;
		counter->counter = their_counter;
	}

	index &= (MELNODE_COUNTER_BITS_TOTAL / BITS_PER_LONG) - 1;
	ret = !test_and_set_bit(their_counter & (BITS_PER_LONG - 1), &counter->backtrack[index]);
	return ret;
}

void melnode_transport_encrypt(u8 *dst, const u8 *plain, size_t plain_len,
				struct melnode_keypair *keypair, u64 *counter_out)
{
	u64 counter = atomic64_inc_return(&keypair->sending_counter) - 1;

	chacha20poly1305_encrypt(dst, plain, plain_len, NULL, 0, counter, keypair->sending.key);
	*counter_out = counter;
}

bool melnode_transport_decrypt(u8 *dst, const u8 *src, size_t src_len, u64 counter,
				struct melnode_keypair *keypair)
{
	return chacha20poly1305_decrypt(dst, src, src_len, NULL, 0, counter, keypair->receiving.key);
}
