/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * Data-message (transport) encrypt/decrypt and replay protection.
 *
 * The replay window (counter_validate, RFC 6479) is ported line-for-line
 * from drivers/net/wireguard/receive.c (Jason A. Donenfeld, GPL-2.0).
 *
 * Deliberately simplified from WireGuard's own transport path: WireGuard
 * builds scatterlists over an skb's fragments so ChaCha20Poly1305 can
 * operate on packet data in place, zero-copy, which matters at WireGuard's
 * scale. melnode encrypts/decrypts into linear buffers instead (skb_copy_
 * bits in, chacha20poly1305_{en,de}crypt, skb straight from that buffer
 * out) - one linearizing copy per packet, on packets already bounded by a
 * <=65000 MTU. That's a real (if modest, at melnode's current traffic
 * expectations) performance cost versus WireGuard's approach; it's not a
 * cost anyone has to accept forever - see melnode_noise.h's header comment
 * on this module's locking for the same "simple now, revisit if it
 * matters" reasoning.
 */
#ifndef _MELNODE_TRANSPORT_H
#define _MELNODE_TRANSPORT_H

#include <linux/types.h>

#include "melnode_noise.h"

/* RFC 6479 sliding-window replay check. Returns true if their_counter is
 * acceptable (and records it as seen); false if it's a replay or too far in
 * the past.
 */
bool melnode_replay_check(struct melnode_replay_counter *counter, u64 their_counter);

/* Encrypts plain_len bytes from plain into dst (which must have room for
 * melnode_noise_encrypted_len(plain_len)), using the given keypair's
 * sending key and the next sending counter (returned in *counter_out for
 * the caller to place in the wire header).
 */
void melnode_transport_encrypt(u8 *dst, const u8 *plain, size_t plain_len,
				struct melnode_keypair *keypair, u64 *counter_out);

/* Decrypts src_len bytes of ciphertext (including the auth tag) from src
 * into dst (which must have room for src_len - MELNODE_NOISE_AUTHTAG_LEN),
 * using the given keypair's receiving key and counter. Does NOT itself call
 * melnode_replay_check() or expiry checks - see melnode_core.c's receive
 * path for the full sequence (key validity/expiry, decrypt, then replay
 * check, matching WireGuard's own ordering in receive.c).
 */
bool melnode_transport_decrypt(u8 *dst, const u8 *src, size_t src_len, u64 counter,
				struct melnode_keypair *keypair);

#endif /* _MELNODE_TRANSPORT_H */
