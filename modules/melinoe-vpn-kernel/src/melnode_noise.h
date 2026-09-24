/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * melnode's Noise_IKpsk2 handshake state machine.
 *
 * Adapted from drivers/net/wireguard/noise.h (Jason A. Donenfeld, GPL-2.0).
 * The math (kdf/mix_hash/mix_dh/message_encrypt/message_decrypt, the
 * initiation/response create/consume state machine) is ported faithfully -
 * melnode-dp's Go implementation is an unmodified fork of wireguard-go, so
 * this is the same Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s construction and
 * must stay bit-for-bit compatible with it.
 *
 * What's deliberately different from WireGuard's kernel implementation:
 *
 *  - No RCU/kref keypair objects and no global index hashtable. WireGuard
 *    needs those because it supports ~2^20 peers with fully random 32-bit
 *    session indices that have to be looked up device-wide. melnode's peer
 *    ids are a fixed 0..255 byte and already double as the array index into
 *    melnode_dev.links, so a session index here is generated as
 *    (random_bits << 8) | peer_id: the low byte IS the lookup. Handshake
 *    and keypair state live directly inside struct melnode_link, guarded by
 *    melnode_dev.lock, not separately allocated/refcounted objects.
 *  - mac1/mac2 cookie-reply DoS mitigation lives in melnode_cookie.[ch] /
 *    melnode_ratelimiter.[ch] (ported from WireGuard's cookie.c/
 *    ratelimiter.c), not here - this file is purely the Noise state
 *    machine, exactly as in WireGuard's own noise.c/noise.h split.
 *  - Single mutex instead of per-object rwsems/kref/RCU. Encrypt/decrypt
 *    still happens outside the lock (see send/receive paths) - only the
 *    keypair metadata (which slot is current, the raw key bytes, the
 *    counter) is touched under it.
 */
#ifndef _MELNODE_NOISE_H
#define _MELNODE_NOISE_H

#include <linux/types.h>

#include "melnode_messages.h"

struct melnode_symmetric_key {
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	u64 birthdate; /* ktime_get_coarse_boottime_ns() */
	bool is_valid;
};

struct melnode_replay_counter {
	u64 counter;
	unsigned long backtrack[MELNODE_COUNTER_BITS_TOTAL / BITS_PER_LONG];
};

struct melnode_keypair {
	bool valid;
	__le32 local_index;  /* what WE call this session; low byte == peer id */
	__le32 remote_index; /* what THEY call it, echoed back to us */
	bool i_am_the_initiator;
	u64 internal_id; /* for logging only */

	struct melnode_symmetric_key sending;
	atomic64_t sending_counter;

	struct melnode_symmetric_key receiving;
	struct melnode_replay_counter receiving_counter;
};

/* current/previous/next, exactly as in WireGuard: a responder's freshly
 * derived keypair isn't trusted for sending until the first successfully
 * decrypted data packet confirms the initiator has it too (see
 * melnode_noise_received_with_keypair).
 */
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
	u64 last_initiation_consumption; /* ktime ns, flood/replay gating */
	u64 last_sent_initiation;        /* ktime ns, our own rate limit */

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

/* One device-wide static identity (DEVICE_SET's key), analogous to
 * WireGuard's struct noise_static_identity - melnode has exactly one
 * device, so this lives directly on melnode_dev rather than as a separate
 * refcounted structure.
 */
void melnode_noise_init(void);

void melnode_noise_handshake_init(struct melnode_handshake *handshake,
				   const u8 peer_public_key[MELNODE_NOISE_PUBLIC_KEY_LEN]);
void melnode_noise_precompute_static_static(struct melnode_handshake *handshake,
					     const u8 local_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
					     const u8 peer_public_key[MELNODE_NOISE_PUBLIC_KEY_LEN]);

bool melnode_noise_handshake_create_initiation(struct melnode_wire_handshake_initiation *dst,
						struct melnode_handshake *handshake,
						const u8 local_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN],
						const u8 local_static_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
						u32 local_index);

/* consume_initiation is split in two, unlike WireGuard's single function:
 * WireGuard identifies the peer via a pubkey hashtable lookup *in between*
 * the Noise steps (right after decrypting the sender's static key, before
 * the "ss" step that needs that peer's precomputed_static_static).
 * melnode.c does that lookup as a linear scan over its <=256-entry link
 * table instead, so phase 1 needs only our own static key and produces the
 * decrypted remote static key plus in-progress crypto state; the caller
 * then looks up (or rejects) the peer and, only on a match, calls phase 2
 * with that peer's handshake (for precomputed_static_static and the
 * replay/flood state, which persist per-peer across attempts).
 */
struct melnode_handshake_precursor {
	u8 chaining_key[MELNODE_NOISE_HASH_LEN];
	u8 hash[MELNODE_NOISE_HASH_LEN];
	u8 remote_ephemeral[MELNODE_NOISE_PUBLIC_KEY_LEN];
	__le32 remote_index;
};

bool melnode_noise_handshake_consume_initiation1(const struct melnode_wire_handshake_initiation *src,
						  const u8 local_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN],
						  const u8 local_static_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
						  u8 remote_static_out[MELNODE_NOISE_PUBLIC_KEY_LEN],
						  struct melnode_handshake_precursor *pre);

bool melnode_noise_handshake_consume_initiation2(const struct melnode_wire_handshake_initiation *src,
						  const struct melnode_handshake_precursor *pre,
						  struct melnode_handshake *handshake);

bool melnode_noise_handshake_create_response(struct melnode_wire_handshake_response *dst,
					      struct melnode_handshake *handshake,
					      u32 local_index);

bool melnode_noise_handshake_consume_response(const struct melnode_wire_handshake_response *src,
					       const u8 local_static_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
					       struct melnode_handshake *handshake);

bool melnode_noise_handshake_begin_session(struct melnode_handshake *handshake,
					    struct melnode_keypairs *keypairs);

/* Call after a data packet successfully decrypts under keypairs->next_kp:
 * promotes next -> current (dropping the old current into previous),
 * confirming to a responder that the initiator has the new keypair too.
 * Simplified from wg_noise_received_with_keypair(): everything here is
 * already under melnode_dev.lock when this is called, so there's no
 * lock-free compare-and-swap dance to replicate.
 */
bool melnode_noise_received_with_keypair(struct melnode_keypairs *keypairs, bool decrypted_with_next);

void melnode_noise_handshake_clear(struct melnode_handshake *handshake);

#endif /* _MELNODE_NOISE_H */
