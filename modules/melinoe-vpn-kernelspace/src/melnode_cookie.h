/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_COOKIE_H
#define _MELNODE_COOKIE_H

#include <linux/types.h>
#include <linux/spinlock.h>

#include "melnode_messages.h"

struct melnode_cookie_checker {
	rwlock_t secret_lock;
	u8 secret[MELNODE_NOISE_HASH_LEN];
	u8 cookie_encryption_key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	u8 message_mac1_key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	u64 secret_birthdate;
};

struct melnode_cookie {
	u64 birthdate;
	bool is_valid;
	u8 cookie[MELNODE_COOKIE_LEN];
	bool have_sent_mac1;
	u8 last_mac1_sent[MELNODE_COOKIE_LEN];
	u8 cookie_decryption_key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
	u8 message_mac1_key[MELNODE_NOISE_SYMMETRIC_KEY_LEN];
};

enum melnode_cookie_mac_state {
	MELNODE_COOKIE_INVALID_MAC,
	MELNODE_COOKIE_VALID_MAC_BUT_NO_COOKIE,
	MELNODE_COOKIE_VALID_MAC_WITH_COOKIE_BUT_RATELIMITED,
	MELNODE_COOKIE_VALID_MAC_WITH_COOKIE,
};

void melnode_cookie_checker_init(struct melnode_cookie_checker *checker);
void melnode_cookie_checker_precompute_device_keys(struct melnode_cookie_checker *checker,
						   const u8 device_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN]);
void melnode_cookie_precompute_peer_keys(struct melnode_cookie *cookie,
					 const u8 peer_static_public[MELNODE_NOISE_PUBLIC_KEY_LEN]);
void melnode_cookie_init(struct melnode_cookie *cookie);

enum melnode_cookie_mac_state melnode_cookie_validate_packet(struct melnode_cookie_checker *checker,
							      const void *message, size_t len,
							      const void *from_addr, int from_len,
							      bool check_cookie);

void melnode_cookie_add_mac_to_packet(void *message, size_t len, struct melnode_cookie *cookie);

void melnode_cookie_message_create(struct melnode_wire_handshake_cookie *dst, const void *message,
				   size_t message_len, const void *from_addr, int from_len,
				   __le32 receiver_index, struct melnode_cookie_checker *checker);
void melnode_cookie_message_consume(const struct melnode_wire_handshake_cookie *src,
				    struct melnode_cookie *cookie);

#endif /* _MELNODE_COOKIE_H */
