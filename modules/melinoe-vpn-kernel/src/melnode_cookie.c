// SPDX-License-Identifier: GPL-2.0-only
/*
 * mac1/mac2 + cookie-reply. Ported from drivers/net/wireguard/cookie.c
 * (Jason A. Donenfeld, GPL-2.0) - see melnode_cookie.h for what's
 * simplified (no per-object rwsems; melnode_dev.lock already covers this
 * state) and the overall design.
 */

#include <linux/string.h>
#include <linux/ktime.h>
#include <linux/in.h>
#include <linux/in6.h>
#include <crypto/blake2s.h>
#include <crypto/chacha20poly1305.h>
#include <crypto/utils.h>

#include "melnode_cookie.h"
#include "melnode_ratelimiter.h"

enum { COOKIE_KEY_LABEL_LEN = 8 };
static const u8 mac1_key_label[COOKIE_KEY_LABEL_LEN] __nonstring = "mac1----";
static const u8 cookie_key_label[COOKIE_KEY_LABEL_LEN] __nonstring = "cookie--";

static bool birthdate_has_expired(u64 birthdate_ns, u64 expiration_seconds)
{
	return (s64)(birthdate_ns + expiration_seconds * NSEC_PER_SEC) <=
	       (s64)ktime_get_coarse_boottime_ns();
}

static void precompute_key(u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN],
			    const u8 pubkey[MELNODE_NOISE_PUBLIC_KEY_LEN],
			    const u8 label[COOKIE_KEY_LABEL_LEN])
{
	struct blake2s_ctx blake;

	blake2s_init(&blake, MELNODE_NOISE_SYMMETRIC_KEY_LEN);
	blake2s_update(&blake, label, COOKIE_KEY_LABEL_LEN);
	blake2s_update(&blake, pubkey, MELNODE_NOISE_PUBLIC_KEY_LEN);
	blake2s_final(&blake, key);
}

void melnode_cookie_checker_init(struct melnode_cookie_checker *checker)
{
	memset(checker, 0, sizeof(*checker));
	checker->secret_birthdate = ktime_get_coarse_boottime_ns();
	get_random_bytes(checker->secret, MELNODE_NOISE_HASH_LEN);
}

void melnode_cookie_checker_precompute_device_keys(struct melnode_cookie_checker *checker,
						    const u8 device_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN])
{
	precompute_key(checker->cookie_encryption_key, device_static_public, cookie_key_label);
	precompute_key(checker->message_mac1_key, device_static_public, mac1_key_label);
}

void melnode_cookie_precompute_peer_keys(struct melnode_cookie *cookie,
					  const u8 peer_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN])
{
	precompute_key(cookie->cookie_decryption_key, peer_static_public, cookie_key_label);
	precompute_key(cookie->message_mac1_key, peer_static_public, mac1_key_label);
}

void melnode_cookie_init(struct melnode_cookie *cookie)
{
	memset(cookie, 0, sizeof(*cookie));
}

static void compute_mac1(u8 mac1[MELNODE_COOKIE_LEN], const void *message, size_t len,
			  const u8 key[MELNODE_NOISE_SYMMETRIC_KEY_LEN])
{
	len = len - sizeof(struct melnode_wire_macs) + offsetof(struct melnode_wire_macs, mac1);
	blake2s(key, MELNODE_NOISE_SYMMETRIC_KEY_LEN, message, len, mac1, MELNODE_COOKIE_LEN);
}

static void compute_mac2(u8 mac2[MELNODE_COOKIE_LEN], const void *message, size_t len,
			  const u8 cookie[MELNODE_COOKIE_LEN])
{
	len = len - sizeof(struct melnode_wire_macs) + offsetof(struct melnode_wire_macs, mac2);
	blake2s(cookie, MELNODE_COOKIE_LEN, message, len, mac2, MELNODE_COOKIE_LEN);
}

static void make_cookie(u8 cookie[MELNODE_COOKIE_LEN], const void *from_addr, int from_len,
			 struct melnode_cookie_checker *checker)
{
	const struct sockaddr *sa = from_addr;
	struct blake2s_ctx blake;

	if (birthdate_has_expired(checker->secret_birthdate, MELNODE_COOKIE_SECRET_MAX_AGE)) {
		checker->secret_birthdate = ktime_get_coarse_boottime_ns();
		get_random_bytes(checker->secret, MELNODE_NOISE_HASH_LEN);
	}

	blake2s_init_key(&blake, MELNODE_COOKIE_LEN, checker->secret, MELNODE_NOISE_HASH_LEN);
	if (sa->sa_family == AF_INET && from_len >= sizeof(struct sockaddr_in)) {
		const struct sockaddr_in *a4 = from_addr;

		blake2s_update(&blake, (const u8 *)&a4->sin_addr, sizeof(a4->sin_addr));
		blake2s_update(&blake, (const u8 *)&a4->sin_port, sizeof(a4->sin_port));
	} else if (sa->sa_family == AF_INET6 && from_len >= sizeof(struct sockaddr_in6)) {
		const struct sockaddr_in6 *a6 = from_addr;

		blake2s_update(&blake, (const u8 *)&a6->sin6_addr, sizeof(a6->sin6_addr));
		blake2s_update(&blake, (const u8 *)&a6->sin6_port, sizeof(a6->sin6_port));
	}
	blake2s_final(&blake, cookie);
}

enum melnode_cookie_mac_state melnode_cookie_validate_packet(struct melnode_cookie_checker *checker,
							       const void *message, size_t len,
							       const void *from_addr, int from_len,
							       bool check_cookie)
{
	const struct melnode_wire_macs *macs =
		(const struct melnode_wire_macs *)((const u8 *)message + len - sizeof(*macs));
	u8 computed_mac[MELNODE_COOKIE_LEN];
	u8 cookie[MELNODE_COOKIE_LEN];

	compute_mac1(computed_mac, message, len, checker->message_mac1_key);
	if (crypto_memneq(computed_mac, macs->mac1, MELNODE_COOKIE_LEN))
		return MELNODE_COOKIE_INVALID_MAC;

	if (!check_cookie)
		return MELNODE_COOKIE_VALID_MAC_BUT_NO_COOKIE;

	make_cookie(cookie, from_addr, from_len, checker);

	compute_mac2(computed_mac, message, len, cookie);
	if (crypto_memneq(computed_mac, macs->mac2, MELNODE_COOKIE_LEN))
		return MELNODE_COOKIE_VALID_MAC_BUT_NO_COOKIE;

	if (!melnode_ratelimiter_allow(from_addr, from_len))
		return MELNODE_COOKIE_VALID_MAC_WITH_COOKIE_BUT_RATELIMITED;

	return MELNODE_COOKIE_VALID_MAC_WITH_COOKIE;
}

void melnode_cookie_add_mac_to_packet(void *message, size_t len, struct melnode_cookie *cookie)
{
	struct melnode_wire_macs *macs = (struct melnode_wire_macs *)((u8 *)message + len - sizeof(*macs));

	compute_mac1(macs->mac1, message, len, cookie->message_mac1_key);
	memcpy(cookie->last_mac1_sent, macs->mac1, MELNODE_COOKIE_LEN);
	cookie->have_sent_mac1 = true;

	if (cookie->is_valid &&
	    !birthdate_has_expired(cookie->birthdate,
				    MELNODE_COOKIE_SECRET_MAX_AGE - MELNODE_COOKIE_SECRET_LATENCY))
		compute_mac2(macs->mac2, message, len, cookie->cookie);
	else
		memset(macs->mac2, 0, MELNODE_COOKIE_LEN);
}

void melnode_cookie_message_create(struct melnode_wire_handshake_cookie *dst, const void *message,
				    size_t message_len, const void *from_addr, int from_len,
				    __le32 receiver_index, struct melnode_cookie_checker *checker)
{
	const struct melnode_wire_macs *macs =
		(const struct melnode_wire_macs *)((const u8 *)message + message_len - sizeof(*macs));
	u8 cookie[MELNODE_COOKIE_LEN];

	dst->header.type = cpu_to_le32(MELNODE_WIRE_MSG_HANDSHAKE_COOKIE);
	dst->receiver_index = receiver_index;
	get_random_bytes(dst->nonce, MELNODE_COOKIE_NONCE_LEN);

	make_cookie(cookie, from_addr, from_len, checker);
	xchacha20poly1305_encrypt(dst->encrypted_cookie, cookie, MELNODE_COOKIE_LEN, macs->mac1,
				   MELNODE_COOKIE_LEN, dst->nonce, checker->cookie_encryption_key);
}

void melnode_cookie_message_consume(const struct melnode_wire_handshake_cookie *src,
				     struct melnode_cookie *cookie)
{
	u8 plain_cookie[MELNODE_COOKIE_LEN];

	if (!cookie->have_sent_mac1)
		return;

	if (xchacha20poly1305_decrypt(plain_cookie, src->encrypted_cookie,
				       sizeof(src->encrypted_cookie), cookie->last_mac1_sent,
				       MELNODE_COOKIE_LEN, src->nonce, cookie->cookie_decryption_key)) {
		memcpy(cookie->cookie, plain_cookie, MELNODE_COOKIE_LEN);
		cookie->birthdate = ktime_get_coarse_boottime_ns();
		cookie->is_valid = true;
		cookie->have_sent_mac1 = false;
	}
}
