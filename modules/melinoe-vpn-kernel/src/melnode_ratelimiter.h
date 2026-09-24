/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * Per-source-address token bucket, gating when melnode_cookie.c demands a
 * mac2/cookie proof from a handshake initiator. Adapted from
 * drivers/net/wireguard/ratelimiter.[ch] (Jason A. Donenfeld, GPL-2.0):
 * same algorithm (siphash-keyed hash table of per-IP token buckets, RCU +
 * periodic GC), with the `struct net *` dimension dropped since melnode is
 * one device in one netns (init_net) - WireGuard supports a device per
 * netns and needs to ratelimit each independently; melnode doesn't.
 *
 * Takes a struct sockaddr_in/sockaddr_in6 directly rather than an skb (see
 * melnode_cookie.h for why - melnode's socket path has no skb to dig one
 * out of).
 */
#ifndef _MELNODE_RATELIMITER_H
#define _MELNODE_RATELIMITER_H

#include <linux/types.h>

int melnode_ratelimiter_init(void);
void melnode_ratelimiter_uninit(void);

/* true = allow, false = drop for being over the per-source-IP rate.
 * from_addr is a struct sockaddr_in or sockaddr_in6 (from_len says which);
 * anything else is refused.
 */
bool melnode_ratelimiter_allow(const void *from_addr, int from_len);

#endif /* _MELNODE_RATELIMITER_H */
