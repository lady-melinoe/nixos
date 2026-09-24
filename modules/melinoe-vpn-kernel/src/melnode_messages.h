/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * melnode's Noise transport wire format.
 *
 * melnode's Noise implementation (cmd/melnode-dp/noise-*.go, cookie.go,
 * keypair.go) is an unmodified fork of wireguard-go's device/ package
 * (diff shows only `package device` -> `package main` and two removed log
 * lines) - so the wire protocol here is exactly WireGuard's Noise_IKpsk2
 * construction. This header is adapted from
 * drivers/net/wireguard/messages.h (Jason A. Donenfeld, GPL-2.0): same
 * structs and constants, renamed to melnode's naming and trimmed to what
 * this module currently uses.
 */
#ifndef _MELNODE_MESSAGES_H
#define _MELNODE_MESSAGES_H

#include <crypto/curve25519.h>
#include <crypto/chacha20poly1305.h>
#include <crypto/blake2s.h>

#include <linux/kernel.h>
#include <linux/types.h>

enum melnode_noise_lengths {
	MELNODE_NOISE_PUBLIC_KEY_LEN = CURVE25519_KEY_SIZE,
	MELNODE_NOISE_SYMMETRIC_KEY_LEN = CHACHA20POLY1305_KEY_SIZE,
	MELNODE_NOISE_TIMESTAMP_LEN = sizeof(u64) + sizeof(u32),
	MELNODE_NOISE_AUTHTAG_LEN = CHACHA20POLY1305_AUTHTAG_SIZE,
	MELNODE_NOISE_HASH_LEN = BLAKE2S_HASH_SIZE,
};

#define melnode_noise_encrypted_len(plain_len) ((plain_len) + MELNODE_NOISE_AUTHTAG_LEN)

enum melnode_cookie_values {
	MELNODE_COOKIE_LEN = 16,
	MELNODE_COOKIE_NONCE_LEN = XCHACHA20POLY1305_NONCE_SIZE,
	MELNODE_COOKIE_SECRET_MAX_AGE = 2 * 60,
	MELNODE_COOKIE_SECRET_LATENCY = 5,
};

enum melnode_counter_values {
	MELNODE_COUNTER_BITS_TOTAL = 8192,
	MELNODE_COUNTER_REDUNDANT_BITS = BITS_PER_LONG,
	MELNODE_COUNTER_WINDOW_SIZE = MELNODE_COUNTER_BITS_TOTAL - MELNODE_COUNTER_REDUNDANT_BITS,
};

enum melnode_noise_limits {
	MELNODE_REKEY_AFTER_MESSAGES = 1ULL << 60,
	MELNODE_REJECT_AFTER_MESSAGES = U64_MAX - MELNODE_COUNTER_WINDOW_SIZE - 1,
	MELNODE_REKEY_AFTER_TIME = 120,
	MELNODE_REJECT_AFTER_TIME = 180,
	MELNODE_REKEY_TIMEOUT = 5,
	MELNODE_INITIATIONS_PER_SECOND = 50,
};

enum melnode_wire_message_type {
	MELNODE_WIRE_MSG_INVALID = 0,
	MELNODE_WIRE_MSG_HANDSHAKE_INITIATION = 1,
	MELNODE_WIRE_MSG_HANDSHAKE_RESPONSE = 2,
	MELNODE_WIRE_MSG_HANDSHAKE_COOKIE = 3,
	MELNODE_WIRE_MSG_DATA = 4,
};

struct melnode_wire_header {
	/* type is a u8 followed by 3 zero bytes; little-endian lets us treat
	 * the whole thing as one u32 (same trick as WireGuard's
	 * message_header).
	 */
	__le32 type;
};

struct melnode_wire_macs {
	u8 mac1[MELNODE_COOKIE_LEN];
	u8 mac2[MELNODE_COOKIE_LEN]; /* always zero - no cookie-reply support yet */
};

struct melnode_wire_handshake_initiation {
	struct melnode_wire_header header;
	__le32 sender_index;
	u8 unencrypted_ephemeral[MELNODE_NOISE_PUBLIC_KEY_LEN];
	u8 encrypted_static[melnode_noise_encrypted_len(MELNODE_NOISE_PUBLIC_KEY_LEN)];
	u8 encrypted_timestamp[melnode_noise_encrypted_len(MELNODE_NOISE_TIMESTAMP_LEN)];
	struct melnode_wire_macs macs;
};

struct melnode_wire_handshake_response {
	struct melnode_wire_header header;
	__le32 sender_index;
	__le32 receiver_index;
	u8 unencrypted_ephemeral[MELNODE_NOISE_PUBLIC_KEY_LEN];
	u8 encrypted_nothing[melnode_noise_encrypted_len(0)];
	struct melnode_wire_macs macs;
};

struct melnode_wire_handshake_cookie {
	struct melnode_wire_header header;
	__le32 receiver_index;
	u8 nonce[MELNODE_COOKIE_NONCE_LEN];
	u8 encrypted_cookie[melnode_noise_encrypted_len(MELNODE_COOKIE_LEN)];
};

struct melnode_wire_data {
	struct melnode_wire_header header;
	__le32 receiver_index;
	__le64 counter;
	u8 encrypted_data[];
};

#define melnode_wire_data_len(plain_len) \
	(melnode_noise_encrypted_len(plain_len) + sizeof(struct melnode_wire_data))

#endif /* _MELNODE_MESSAGES_H */
