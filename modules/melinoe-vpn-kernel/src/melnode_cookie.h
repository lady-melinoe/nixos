/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * mac1/mac2 message authentication and the cookie-reply DoS mitigation.
 * Ported from drivers/net/wireguard/cookie.[ch] (Jason A. Donenfeld,
 * GPL-2.0): mac1 is a mandatory, cheap, cookie-independent authenticator
 * every handshake message carries; mac2 is added only once a responder
 * decides it's under load (see melnode_core.c's handshake receive path) and
 * the initiator has proven, via a cookie the responder handed it, that it
 * can actually receive traffic at its claimed source address - the actual
 * spoofed-source flood mitigation. melnode_ratelimiter.[ch] gates mac2
 * validation itself by source IP on top of that.
 *
 * struct melnode_cookie_checker is device-wide (one per melnode_dev);
 * struct melnode_cookie is per-link. Both are protected by melnode_dev.lock
 * like the rest of this module's state - see melnode_noise.h's header
 * comment for why that's an acceptable simplification here.
 *
 * Unlike WireGuard's cookie.c, these take the source address directly
 * (a struct sockaddr_in/sockaddr_in6) rather than digging saddr/source-port
 * out of an skb: melnode's socket path (melnode_socket.c) receives via
 * kernel_recvmsg into a flat buffer, not skb-based receive, so there is no
 * skb to dig them out of - and the sockaddr already has exactly the two
 * fields make_cookie() hashes (address, source port).
 */
#ifndef _MELNODE_COOKIE_H
#define _MELNODE_COOKIE_H

#include <linux/types.h>

#include "melnode_messages.h"

struct melnode_cookie_checker {
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

/* message/len is the whole handshake message (header included), mac1/mac2
 * sitting at its tail exactly as in struct melnode_wire_macs. from_addr is
 * a struct sockaddr_in or sockaddr_in6 (from_len says which).
 */
enum melnode_cookie_mac_state melnode_cookie_validate_packet(struct melnode_cookie_checker *checker,
							       const void *message, size_t len,
							       const void *from_addr, int from_len,
							       bool check_cookie);

void melnode_cookie_add_mac_to_packet(void *message, size_t len, struct melnode_cookie *cookie);

void melnode_cookie_message_create(struct melnode_wire_handshake_cookie *dst, const void *message,
				    size_t message_len, const void *from_addr, int from_len,
				    __le32 receiver_index, struct melnode_cookie_checker *checker);
/* Caller has already used src->receiver_index (low byte == peer id in
 * melnode's index scheme) to find the right link's cookie state.
 */
void melnode_cookie_message_consume(const struct melnode_wire_handshake_cookie *src,
				     struct melnode_cookie *cookie);

#endif /* _MELNODE_COOKIE_H */
