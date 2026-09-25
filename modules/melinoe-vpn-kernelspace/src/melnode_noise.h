/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_NOISE_H
#define _MELNODE_NOISE_H

#include <linux/types.h>
#include <linux/ktime.h>

#include "melnode_messages.h"

struct melnode_symmetric_key {
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	u64 birthdate;
	bool is_valid;
};

struct melnode_replay_counter {
	u64 counter;
	unsigned long backtrack[MELNODE_COUNTER_BITS_TOTAL / BITS_PER_LONG];
};

static inline bool melnode_key_expired(const struct melnode_symmetric_key *key, u64 seconds)
{
	return (s64)(key->birthdate + seconds * NSEC_PER_SEC) <=
	       (s64)ktime_get_coarse_boottime_ns();
}

struct melnode_keypair {
	bool valid;
	bool initiator;
	__le32 local_index;
	__le32 remote_index;

	struct melnode_symmetric_key sending;
	u64 sending_counter;

	struct melnode_symmetric_key receiving;
	struct melnode_replay_counter receiving_counter;
};

struct melnode_keypairs {
	struct melnode_keypair current_kp;
	struct melnode_keypair previous_kp;
	struct melnode_keypair next_kp;
};

enum melnode_handshake_state {
	MELNODE_HANDSHAKE_ZEROED = 0,
	MELNODE_HANDSHAKE_CREATED_INITIATION,
	MELNODE_HANDSHAKE_CONSUMED_INITIATION,
	MELNODE_HANDSHAKE_CREATED_RESPONSE,
	MELNODE_HANDSHAKE_CONSUMED_RESPONSE,
};

struct melnode_handshake {
	enum melnode_handshake_state state;
	u64 last_initiation_consumption;
	u64 last_sent_initiation;

	u8 ephemeral_private[MELNODE_NOISE_PUBLIC_KEY_LEN];
	u8 remote_static[MELNODE_NOISE_PUBLIC_KEY_LEN];
	u8 remote_ephemeral[MELNODE_NOISE_PUBLIC_KEY_LEN];
	u8 precomputed_static_static[MELNODE_NOISE_PUBLIC_KEY_LEN];

	u8 hash[MELNODE_NOISE_HASH_LEN];
	u8 chaining_key[MELNODE_NOISE_HASH_LEN];

	u8 latest_timestamp[MELNODE_NOISE_TIMESTAMP_LEN];
	__le32 local_index;
	__le32 remote_index;
};

struct melnode_handshake_precursor {
	u8 chaining_key[MELNODE_NOISE_HASH_LEN];
	u8 hash[MELNODE_NOISE_HASH_LEN];
	u8 remote_ephemeral[MELNODE_NOISE_PUBLIC_KEY_LEN];
	__le32 remote_index;
};

void melnode_noise_init(void);

void melnode_noise_handshake_init(struct melnode_handshake *handshake,
				  const u8 peer_public_key[MELNODE_NOISE_PUBLIC_KEY_LEN]);
void melnode_noise_precompute_static_static(struct melnode_handshake *handshake,
					    const u8 local_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
					    const u8 peer_public_key[MELNODE_NOISE_PUBLIC_KEY_LEN]);

bool melnode_noise_handshake_create_initiation(struct melnode_wire_handshake_initiation *dst,
					       struct melnode_handshake *handshake,
					       const u8 local_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN],
					       u32 local_index);

bool melnode_noise_handshake_consume_initiation1(const struct melnode_wire_handshake_initiation *src,
						 const u8 local_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN],
						 const u8 local_static_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
						 u8 remote_static_out[MELNODE_NOISE_PUBLIC_KEY_LEN],
						 struct melnode_handshake_precursor *pre);

bool melnode_noise_handshake_consume_initiation2(const struct melnode_wire_handshake_initiation *src,
						 const struct melnode_handshake_precursor *pre,
						 struct melnode_handshake *handshake);

bool melnode_noise_handshake_create_response(struct melnode_wire_handshake_response *dst,
					     struct melnode_handshake *handshake, u32 local_index);

bool melnode_noise_handshake_consume_response(const struct melnode_wire_handshake_response *src,
					      const u8 local_static_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
					      struct melnode_handshake *handshake);

bool melnode_noise_handshake_begin_session(struct melnode_handshake *handshake,
					   struct melnode_keypairs *keypairs);

bool melnode_noise_received_with_keypair(struct melnode_keypairs *keypairs);

void melnode_noise_handshake_clear(struct melnode_handshake *handshake);
void melnode_noise_keypairs_clear(struct melnode_keypairs *keypairs);

#endif
