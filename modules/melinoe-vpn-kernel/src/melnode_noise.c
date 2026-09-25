// SPDX-License-Identifier: GPL-2.0-only
/*
 * melnode's Noise_IKpsk2 handshake state machine.
 *
 * The KDF/mix/encrypt primitives and the initiation/response create/consume
 * control flow are ported line-for-line from drivers/net/wireguard/noise.c
 * (Jason A. Donenfeld, GPL-2.0) - see melnode_noise.h for what's simplified
 * (no RCU/kref/index-hashtable) and why that's safe here. melnode-dp's Go
 * implementation is an unmodified fork of wireguard-go, so this must match
 * it bit-for-bit.
 *
 * melnode currently has no preshared-key concept (LINK_ADD only carries a
 * public key), so the "psk" input to Noise_IKpsk2 is always the all-zero
 * key - mix_psk() still runs (it's part of the fixed construction string
 * "Noise_IKpsk2_..."), just with an all-zero psk, identical to how
 * WireGuard itself behaves when a peer has no preshared key configured.
 *
 * consume_initiation is split into two phases here, unlike WireGuard's
 * single wg_noise_handshake_consume_initiation(): WireGuard identifies the
 * peer via a pubkey hashtable lookup *in the middle* of the Noise steps
 * (after decrypting the sender's static key, before the "ss" step, which
 * needs that peer's precomputed_static_static). melnode.c does that lookup
 * as a linear scan over its <=256-entry link table instead, so the split
 * happens at exactly that point: phase 1 needs only our own static key and
 * produces the decrypted remote static key; the caller looks up the link;
 * phase 2 takes that link's handshake state and finishes the exchange.
 */

#include <linux/string.h>
#include <linux/ktime.h>
#include <linux/kernel.h>
#include <crypto/curve25519.h>
#include <crypto/chacha20poly1305.h>
#include <crypto/blake2s.h>
#include <crypto/utils.h>

#include "melnode_noise.h"

/* Identical strings to WireGuard's own - this is still literally the
 * Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s construction (see noise-protocol.go:
 * NoiseConstruction / WGIdentifier), so the derived init hash/chaining key
 * must match exactly.
 */
static const u8 handshake_name[37] __nonstring = "Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s";
static const u8 identifier_name[34] __nonstring = "WireGuard v1 zx2c4 Jason@zx2c4.com";
static u8 handshake_init_hash[MELNODE_NOISE_HASH_LEN] __ro_after_init;
static u8 handshake_init_chaining_key[MELNODE_NOISE_HASH_LEN] __ro_after_init;

void melnode_noise_init(void)
{
	struct blake2s_ctx blake;

	blake2s(NULL, 0, handshake_name, sizeof(handshake_name), handshake_init_chaining_key,
		MELNODE_NOISE_HASH_LEN);
	blake2s_init(&blake, MELNODE_NOISE_HASH_LEN);
	blake2s_update(&blake, handshake_init_chaining_key, MELNODE_NOISE_HASH_LEN);
	blake2s_update(&blake, identifier_name, sizeof(identifier_name));
	blake2s_final(&blake, handshake_init_hash);
}

void melnode_noise_precompute_static_static(struct melnode_handshake *handshake,
					     const u8 local_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
					     const u8 peer_public_key[MELNODE_NOISE_PUBLIC_KEY_LEN])
{
	if (!curve25519(handshake->precomputed_static_static, local_private, peer_public_key))
		memset(handshake->precomputed_static_static, 0, MELNODE_NOISE_PUBLIC_KEY_LEN);
}

void melnode_noise_handshake_init(struct melnode_handshake *handshake,
				   const u8 peer_public_key[MELNODE_NOISE_PUBLIC_KEY_LEN])
{
	memset(handshake, 0, sizeof(*handshake));
	memcpy(handshake->remote_static, peer_public_key, MELNODE_NOISE_PUBLIC_KEY_LEN);
	handshake->state = MELNODE_HANDSHAKE_ZEROED;
	/* precomputed_static_static is filled in by a separate call to
	 * melnode_noise_precompute_static_static() (it needs the device's
	 * private key, which this function deliberately doesn't take).
	 */
}

static void handshake_zero(struct melnode_handshake *handshake)
{
	memzero_explicit(handshake->ephemeral_private, MELNODE_NOISE_PUBLIC_KEY_LEN);
	memset(handshake->remote_ephemeral, 0, MELNODE_NOISE_PUBLIC_KEY_LEN);
	memset(handshake->hash, 0, MELNODE_NOISE_HASH_LEN);
	memset(handshake->chaining_key, 0, MELNODE_NOISE_HASH_LEN);
	handshake->remote_index = 0;
	handshake->state = MELNODE_HANDSHAKE_ZEROED;
}

void melnode_noise_handshake_clear(struct melnode_handshake *handshake)
{
	handshake_zero(handshake);
}

/* ------------------------------------------------------------------------
 * KDF/mix primitives - verbatim math from wg-noise.c.
 * ------------------------------------------------------------------------ */

static void hmac(u8 *out, const u8 *in, const u8 *key, size_t inlen, size_t keylen)
{
	struct blake2s_ctx blake;
	u8 x_key[BLAKE2S_BLOCK_SIZE] __aligned(__alignof__(u32)) = { 0 };
	u8 i_hash[BLAKE2S_HASH_SIZE] __aligned(__alignof__(u32));
	int i;

	if (keylen > BLAKE2S_BLOCK_SIZE) {
		blake2s_init(&blake, BLAKE2S_HASH_SIZE);
		blake2s_update(&blake, key, keylen);
		blake2s_final(&blake, x_key);
	} else {
		memcpy(x_key, key, keylen);
	}

	for (i = 0; i < BLAKE2S_BLOCK_SIZE; ++i)
		x_key[i] ^= 0x36;

	blake2s_init(&blake, BLAKE2S_HASH_SIZE);
	blake2s_update(&blake, x_key, BLAKE2S_BLOCK_SIZE);
	blake2s_update(&blake, in, inlen);
	blake2s_final(&blake, i_hash);

	for (i = 0; i < BLAKE2S_BLOCK_SIZE; ++i)
		x_key[i] ^= 0x5c ^ 0x36;

	blake2s_init(&blake, BLAKE2S_HASH_SIZE);
	blake2s_update(&blake, x_key, BLAKE2S_BLOCK_SIZE);
	blake2s_update(&blake, i_hash, BLAKE2S_HASH_SIZE);
	blake2s_final(&blake, i_hash);

	memcpy(out, i_hash, BLAKE2S_HASH_SIZE);
	memzero_explicit(x_key, BLAKE2S_BLOCK_SIZE);
	memzero_explicit(i_hash, BLAKE2S_HASH_SIZE);
}

/* Hugo Krawczyk's HKDF: https://eprint.iacr.org/2010/264.pdf */
static void kdf(u8 *first_dst, u8 *second_dst, u8 *third_dst, const u8 *data, size_t first_len,
		 size_t second_len, size_t third_len, size_t data_len,
		 const u8 chaining_key[MELNODE_NOISE_HASH_LEN])
{
	u8 output[BLAKE2S_HASH_SIZE + 1];
	u8 secret[BLAKE2S_HASH_SIZE];

	hmac(secret, data, chaining_key, data_len, MELNODE_NOISE_HASH_LEN);

	if (!first_dst || !first_len)
		goto out;

	output[0] = 1;
	hmac(output, output, secret, 1, BLAKE2S_HASH_SIZE);
	memcpy(first_dst, output, first_len);

	if (!second_dst || !second_len)
		goto out;

	output[BLAKE2S_HASH_SIZE] = 2;
	hmac(output, output, secret, BLAKE2S_HASH_SIZE + 1, BLAKE2S_HASH_SIZE);
	memcpy(second_dst, output, second_len);

	if (!third_dst || !third_len)
		goto out;

	output[BLAKE2S_HASH_SIZE] = 3;
	hmac(output, output, secret, BLAKE2S_HASH_SIZE + 1, BLAKE2S_HASH_SIZE);
	memcpy(third_dst, output, third_len);

out:
	memzero_explicit(secret, BLAKE2S_HASH_SIZE);
	memzero_explicit(output, BLAKE2S_HASH_SIZE + 1);
}

static void derive_keys(struct melnode_symmetric_key *first_dst,
			 struct melnode_symmetric_key *second_dst,
			 const u8 chaining_key[MELNODE_NOISE_HASH_LEN])
{
	u64 birthdate = ktime_get_coarse_boottime_ns();

	kdf(first_dst->key, second_dst->key, NULL, NULL, MELNODE_NOISE_SYMMETRIC_KEY_LEN,
	    MELNODE_NOISE_SYMMETRIC_KEY_LEN, 0, 0, chaining_key);
	first_dst->birthdate = second_dst->birthdate = birthdate;
	first_dst->is_valid = second_dst->is_valid = true;
}

static bool __must_check mix_dh(u8 chaining_key[MELNODE_NOISE_HASH_LEN],
				 u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN],
				 const u8 private_key[MELNODE_NOISE_PUBLIC_KEY_LEN],
				 const u8 public_key[MELNODE_NOISE_PUBLIC_KEY_LEN])
{
	u8 dh_calculation[MELNODE_NOISE_PUBLIC_KEY_LEN];

	if (unlikely(!curve25519(dh_calculation, private_key, public_key)))
		return false;
	kdf(chaining_key, key, NULL, dh_calculation, MELNODE_NOISE_HASH_LEN,
	    MELNODE_NOISE_SYMMETRIC_KEY_LEN, 0, MELNODE_NOISE_PUBLIC_KEY_LEN, chaining_key);
	memzero_explicit(dh_calculation, MELNODE_NOISE_PUBLIC_KEY_LEN);
	return true;
}

static bool __must_check mix_precomputed_dh(u8 chaining_key[MELNODE_NOISE_HASH_LEN],
					     u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN],
					     const u8 precomputed[MELNODE_NOISE_PUBLIC_KEY_LEN])
{
	static const u8 zero_point[MELNODE_NOISE_PUBLIC_KEY_LEN];

	if (unlikely(!crypto_memneq(precomputed, zero_point, MELNODE_NOISE_PUBLIC_KEY_LEN)))
		return false;
	kdf(chaining_key, key, NULL, precomputed, MELNODE_NOISE_HASH_LEN,
	    MELNODE_NOISE_SYMMETRIC_KEY_LEN, 0, MELNODE_NOISE_PUBLIC_KEY_LEN, chaining_key);
	return true;
}

static void mix_hash(u8 hash[MELNODE_NOISE_HASH_LEN], const u8 *src, size_t src_len)
{
	struct blake2s_ctx blake;

	blake2s_init(&blake, MELNODE_NOISE_HASH_LEN);
	blake2s_update(&blake, hash, MELNODE_NOISE_HASH_LEN);
	blake2s_update(&blake, src, src_len);
	blake2s_final(&blake, hash);
}

static void mix_psk(u8 chaining_key[MELNODE_NOISE_HASH_LEN], u8 hash[MELNODE_NOISE_HASH_LEN],
		     u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN],
		     const u8 psk[MELNODE_NOISE_SYMMETRIC_KEY_LEN])
{
	u8 temp_hash[MELNODE_NOISE_HASH_LEN];

	kdf(chaining_key, temp_hash, key, psk, MELNODE_NOISE_HASH_LEN, MELNODE_NOISE_HASH_LEN,
	    MELNODE_NOISE_SYMMETRIC_KEY_LEN, MELNODE_NOISE_SYMMETRIC_KEY_LEN, chaining_key);
	mix_hash(hash, temp_hash, MELNODE_NOISE_HASH_LEN);
	memzero_explicit(temp_hash, MELNODE_NOISE_HASH_LEN);
}

static void handshake_init(u8 chaining_key[MELNODE_NOISE_HASH_LEN], u8 hash[MELNODE_NOISE_HASH_LEN],
			    const u8 remote_static[MELNODE_NOISE_PUBLIC_KEY_LEN])
{
	memcpy(hash, handshake_init_hash, MELNODE_NOISE_HASH_LEN);
	memcpy(chaining_key, handshake_init_chaining_key, MELNODE_NOISE_HASH_LEN);
	mix_hash(hash, remote_static, MELNODE_NOISE_PUBLIC_KEY_LEN);
}

static void message_encrypt(u8 *dst_ciphertext, const u8 *src_plaintext, size_t src_len,
			     u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN], u8 hash[MELNODE_NOISE_HASH_LEN])
{
	chacha20poly1305_encrypt(dst_ciphertext, src_plaintext, src_len, hash,
				  MELNODE_NOISE_HASH_LEN, 0 /* always zero for Noise_IK */, key);
	mix_hash(hash, dst_ciphertext, melnode_noise_encrypted_len(src_len));
}

static bool message_decrypt(u8 *dst_plaintext, const u8 *src_ciphertext, size_t src_len,
			     u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN], u8 hash[MELNODE_NOISE_HASH_LEN])
{
	if (!chacha20poly1305_decrypt(dst_plaintext, src_ciphertext, src_len, hash,
				       MELNODE_NOISE_HASH_LEN, 0 /* always zero for Noise_IK */, key))
		return false;
	mix_hash(hash, src_ciphertext, src_len);
	return true;
}

static void message_ephemeral(u8 ephemeral_dst[MELNODE_NOISE_PUBLIC_KEY_LEN],
			       const u8 ephemeral_src[MELNODE_NOISE_PUBLIC_KEY_LEN],
			       u8 chaining_key[MELNODE_NOISE_HASH_LEN], u8 hash[MELNODE_NOISE_HASH_LEN])
{
	if (ephemeral_dst != ephemeral_src)
		memcpy(ephemeral_dst, ephemeral_src, MELNODE_NOISE_PUBLIC_KEY_LEN);
	mix_hash(hash, ephemeral_src, MELNODE_NOISE_PUBLIC_KEY_LEN);
	kdf(chaining_key, NULL, NULL, ephemeral_src, MELNODE_NOISE_HASH_LEN, 0, 0,
	    MELNODE_NOISE_PUBLIC_KEY_LEN, chaining_key);
}

static void tai64n_now(u8 output[MELNODE_NOISE_TIMESTAMP_LEN])
{
	struct timespec64 now;

	ktime_get_real_ts64(&now);
	/* Round down to the same granularity WireGuard uses, to avoid an
	 * infoleak from a precise clock.
	 */
	now.tv_nsec = ALIGN_DOWN(now.tv_nsec,
				  rounddown_pow_of_two(NSEC_PER_SEC / MELNODE_INITIATIONS_PER_SECOND));
	*(__be64 *)output = cpu_to_be64(0x400000000000000aULL + now.tv_sec);
	*(__be32 *)(output + sizeof(__be64)) = cpu_to_be32(now.tv_nsec);
}

/* ------------------------------------------------------------------------
 * Handshake message create/consume.
 * ------------------------------------------------------------------------ */

bool melnode_noise_handshake_create_initiation(struct melnode_wire_handshake_initiation *dst,
						struct melnode_handshake *handshake,
						const u8 local_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN],
						const u8 local_static_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
						u32 local_index)
{
	u8 timestamp[MELNODE_NOISE_TIMESTAMP_LEN];
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	bool ret = false;

	dst->header.type = cpu_to_le32(MELNODE_WIRE_MSG_HANDSHAKE_INITIATION);

	handshake_init(handshake->chaining_key, handshake->hash, handshake->remote_static);

	/* e */
	curve25519_generate_secret(handshake->ephemeral_private);
	if (!curve25519_generate_public(dst->unencrypted_ephemeral, handshake->ephemeral_private))
		goto out;
	message_ephemeral(dst->unencrypted_ephemeral, dst->unencrypted_ephemeral,
			   handshake->chaining_key, handshake->hash);

	/* es */
	if (!mix_dh(handshake->chaining_key, key, handshake->ephemeral_private,
		    handshake->remote_static))
		goto out;

	/* s */
	message_encrypt(dst->encrypted_static, local_static_public, MELNODE_NOISE_PUBLIC_KEY_LEN,
			 key, handshake->hash);

	/* ss */
	if (!mix_precomputed_dh(handshake->chaining_key, key, handshake->precomputed_static_static))
		goto out;

	/* {t} */
	tai64n_now(timestamp);
	message_encrypt(dst->encrypted_timestamp, timestamp, MELNODE_NOISE_TIMESTAMP_LEN, key,
			 handshake->hash);

	dst->sender_index = cpu_to_le32(local_index);
	handshake->local_index = dst->sender_index;
	handshake->state = MELNODE_HANDSHAKE_CREATED_INITIATION;
	ret = true;

out:
	(void)local_static_private;
	memzero_explicit(key, MELNODE_NOISE_SYMMETRIC_KEY_LEN);
	memzero_explicit(timestamp, MELNODE_NOISE_TIMESTAMP_LEN);
	return ret;
}

/* Phase 1: needs only our own static key. Produces the peer's decrypted
 * static key (for the caller to look up) and the in-progress chaining_key/
 * hash/remote_ephemeral/remote_index, none of which are peer-specific yet.
 */
bool melnode_noise_handshake_consume_initiation1(const struct melnode_wire_handshake_initiation *src,
						  const u8 local_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN],
						  const u8 local_static_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
						  u8 remote_static_out[MELNODE_NOISE_PUBLIC_KEY_LEN],
						  struct melnode_handshake_precursor *pre)
{
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	bool ret = false;

	handshake_init(pre->chaining_key, pre->hash, local_static_public);

	/* e */
	message_ephemeral(pre->remote_ephemeral, src->unencrypted_ephemeral, pre->chaining_key,
			   pre->hash);

	/* es */
	if (!mix_dh(pre->chaining_key, key, local_static_private, pre->remote_ephemeral))
		goto out;

	/* s */
	if (!message_decrypt(remote_static_out, src->encrypted_static,
			      sizeof(src->encrypted_static), key, pre->hash))
		goto out;

	pre->remote_index = src->sender_index;
	ret = true;

out:
	memzero_explicit(key, MELNODE_NOISE_SYMMETRIC_KEY_LEN);
	return ret;
}

/* Phase 2: caller has identified the peer (remote_static_out from phase 1
 * matched a known link) and hands us that peer's own handshake state, for
 * precomputed_static_static and the persistent replay/flood-gating fields
 * (latest_timestamp, last_initiation_consumption). The {t} ciphertext is
 * re-derived from *pre* rather than threaded through as a separate
 * argument, since decrypting it needs pre->hash/chaining_key up to the "ss"
 * step - so this takes the original wire message too.
 */
bool melnode_noise_handshake_consume_initiation2(const struct melnode_wire_handshake_initiation *src,
						  const struct melnode_handshake_precursor *pre,
						  struct melnode_handshake *handshake)
{
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	u8 chaining_key[MELNODE_NOISE_HASH_LEN];
	u8 hash[MELNODE_NOISE_HASH_LEN];
	u8 timestamp[MELNODE_NOISE_TIMESTAMP_LEN];
	u64 now;
	bool ret = false;

	memcpy(chaining_key, pre->chaining_key, MELNODE_NOISE_HASH_LEN);
	memcpy(hash, pre->hash, MELNODE_NOISE_HASH_LEN);

	/* ss */
	if (!mix_precomputed_dh(chaining_key, key, handshake->precomputed_static_static))
		goto out;

	/* {t} */
	if (!message_decrypt(timestamp, src->encrypted_timestamp, sizeof(src->encrypted_timestamp),
			      key, hash))
		goto out;

	/* Replay and flood gating, exactly as wg_noise_handshake_consume_
	 * initiation(): a strictly-increasing timestamp per peer, and a
	 * minimum spacing between accepted initiations from the same peer.
	 * This is per-peer bookkeeping, distinct from the global
	 * cookie/ratelimiter DoS mitigation in melnode_cookie.c, which
	 * limits by *source address* before we even get this far.
	 */
	now = ktime_get_coarse_boottime_ns();
	if (memcmp(timestamp, handshake->latest_timestamp, MELNODE_NOISE_TIMESTAMP_LEN) <= 0)
		goto out; /* replay */
	if ((s64)handshake->last_initiation_consumption + NSEC_PER_SEC / MELNODE_INITIATIONS_PER_SECOND >
	    (s64)now)
		goto out; /* flood from this peer specifically */

	memcpy(handshake->remote_ephemeral, pre->remote_ephemeral, MELNODE_NOISE_PUBLIC_KEY_LEN);
	memcpy(handshake->latest_timestamp, timestamp, MELNODE_NOISE_TIMESTAMP_LEN);
	memcpy(handshake->hash, hash, MELNODE_NOISE_HASH_LEN);
	memcpy(handshake->chaining_key, chaining_key, MELNODE_NOISE_HASH_LEN);
	handshake->remote_index = pre->remote_index;
	handshake->last_initiation_consumption = now;
	handshake->state = MELNODE_HANDSHAKE_CONSUMED_INITIATION;
	ret = true;

out:
	memzero_explicit(key, MELNODE_NOISE_SYMMETRIC_KEY_LEN);
	memzero_explicit(hash, MELNODE_NOISE_HASH_LEN);
	memzero_explicit(chaining_key, MELNODE_NOISE_HASH_LEN);
	memzero_explicit(timestamp, MELNODE_NOISE_TIMESTAMP_LEN);
	return ret;
}

bool melnode_noise_handshake_create_response(struct melnode_wire_handshake_response *dst,
					      struct melnode_handshake *handshake,
					      u32 local_index)
{
	static const u8 zero_psk[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	bool ret = false;

	if (handshake->state != MELNODE_HANDSHAKE_CONSUMED_INITIATION)
		return false;

	dst->header.type = cpu_to_le32(MELNODE_WIRE_MSG_HANDSHAKE_RESPONSE);
	dst->receiver_index = handshake->remote_index;

	/* e */
	curve25519_generate_secret(handshake->ephemeral_private);
	if (!curve25519_generate_public(dst->unencrypted_ephemeral, handshake->ephemeral_private))
		goto out;
	message_ephemeral(dst->unencrypted_ephemeral, dst->unencrypted_ephemeral,
			   handshake->chaining_key, handshake->hash);

	/* ee */
	if (!mix_dh(handshake->chaining_key, NULL, handshake->ephemeral_private,
		    handshake->remote_ephemeral))
		goto out;

	/* se */
	if (!mix_dh(handshake->chaining_key, NULL, handshake->ephemeral_private,
		    handshake->remote_static))
		goto out;

	/* psk (always zero - melnode has no preshared-key concept yet) */
	mix_psk(handshake->chaining_key, handshake->hash, key, zero_psk);

	/* {} */
	message_encrypt(dst->encrypted_nothing, NULL, 0, key, handshake->hash);

	dst->sender_index = cpu_to_le32(local_index);
	handshake->local_index = dst->sender_index;
	handshake->state = MELNODE_HANDSHAKE_CREATED_RESPONSE;
	ret = true;

out:
	memzero_explicit(key, MELNODE_NOISE_SYMMETRIC_KEY_LEN);
	return ret;
}

bool melnode_noise_handshake_consume_response(const struct melnode_wire_handshake_response *src,
					       const u8 local_static_private[MELNODE_NOISE_PUBLIC_KEY_LEN],
					       struct melnode_handshake *handshake)
{
	static const u8 zero_psk[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	enum melnode_handshake_state state = handshake->state;
	u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	u8 hash[MELNODE_NOISE_HASH_LEN];
	u8 chaining_key[MELNODE_NOISE_HASH_LEN];
	u8 e[MELNODE_NOISE_PUBLIC_KEY_LEN];
	bool ret = false;

	if (state != MELNODE_HANDSHAKE_CREATED_INITIATION)
		return false;

	memcpy(hash, handshake->hash, MELNODE_NOISE_HASH_LEN);
	memcpy(chaining_key, handshake->chaining_key, MELNODE_NOISE_HASH_LEN);

	/* e */
	message_ephemeral(e, src->unencrypted_ephemeral, chaining_key, hash);

	/* ee */
	if (!mix_dh(chaining_key, NULL, handshake->ephemeral_private, e))
		goto out;

	/* se */
	if (!mix_dh(chaining_key, NULL, local_static_private, e))
		goto out;

	/* psk */
	mix_psk(chaining_key, hash, key, zero_psk);

	/* {} */
	if (!message_decrypt(NULL, src->encrypted_nothing, sizeof(src->encrypted_nothing), key, hash))
		goto out;

	if (handshake->state != state)
		goto out; /* raced with something else touching this handshake */

	memcpy(handshake->remote_ephemeral, e, MELNODE_NOISE_PUBLIC_KEY_LEN);
	memcpy(handshake->hash, hash, MELNODE_NOISE_HASH_LEN);
	memcpy(handshake->chaining_key, chaining_key, MELNODE_NOISE_HASH_LEN);
	handshake->remote_index = src->sender_index;
	handshake->state = MELNODE_HANDSHAKE_CONSUMED_RESPONSE;
	ret = true;

out:
	memzero_explicit(key, MELNODE_NOISE_SYMMETRIC_KEY_LEN);
	memzero_explicit(hash, MELNODE_NOISE_HASH_LEN);
	memzero_explicit(chaining_key, MELNODE_NOISE_HASH_LEN);
	memzero_explicit(e, MELNODE_NOISE_PUBLIC_KEY_LEN);
	return ret;
}

/* Slots the freshly derived keypair into current/previous/next, exactly as
 * wg_noise_handshake_begin_session() / add_new_keypair() do - see the
 * comment on struct melnode_keypairs. Caller holds the relevant link's own
 * lock (melnode_link.lock - see melnode_core.h).
 */
bool melnode_noise_handshake_begin_session(struct melnode_handshake *handshake,
					    struct melnode_keypairs *keypairs)
{
	struct melnode_keypair new_keypair = { 0 };

	if (handshake->state != MELNODE_HANDSHAKE_CREATED_RESPONSE &&
	    handshake->state != MELNODE_HANDSHAKE_CONSUMED_RESPONSE)
		return false;

	new_keypair.valid = true;
	new_keypair.i_am_the_initiator = handshake->state == MELNODE_HANDSHAKE_CONSUMED_RESPONSE;
	new_keypair.remote_index = handshake->remote_index;
	new_keypair.local_index = handshake->local_index;
	atomic64_set(&new_keypair.sending_counter, 0);

	if (new_keypair.i_am_the_initiator)
		derive_keys(&new_keypair.sending, &new_keypair.receiving, handshake->chaining_key);
	else
		derive_keys(&new_keypair.receiving, &new_keypair.sending, handshake->chaining_key);

	handshake_zero(handshake);

	if (new_keypair.i_am_the_initiator) {
		/* We sent the initiation, got a response: this keypair is
		 * immediately usable for sending.
		 */
		if (keypairs->next_kp.valid) {
			keypairs->previous_kp = keypairs->next_kp;
			memset(&keypairs->next_kp, 0, sizeof(keypairs->next_kp));
		} else {
			keypairs->previous_kp = keypairs->current_kp;
		}
		keypairs->current_kp = new_keypair;
	} else {
		/* We're the responder: not trusted for sending until the
		 * first successfully decrypted data packet confirms the
		 * initiator has it too (melnode_noise_received_with_keypair).
		 */
		keypairs->next_kp = new_keypair;
		memset(&keypairs->previous_kp, 0, sizeof(keypairs->previous_kp));
	}
	return true;
}

bool melnode_noise_received_with_keypair(struct melnode_keypairs *keypairs, bool decrypted_with_next)
{
	if (!decrypted_with_next || !keypairs->next_kp.valid)
		return false;
	keypairs->previous_kp = keypairs->current_kp;
	keypairs->current_kp = keypairs->next_kp;
	memset(&keypairs->next_kp, 0, sizeof(keypairs->next_kp));
	return true;
}
