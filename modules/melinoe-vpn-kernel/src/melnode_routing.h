/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_ROUTING_H
#define _MELNODE_ROUTING_H

#include <linux/types.h>

struct melnode_link;

#define MELNODE_HDR_LEN 4
#define MELNODE_HDR_OFF_PROTO 0
#define MELNODE_HDR_OFF_SRC 1
#define MELNODE_HDR_OFF_DST 2
#define MELNODE_HDR_OFF_TTL 3
#define MELNODE_DEFAULT_TTL 64

int melnode_routing_link_init(struct melnode_link *link);

struct melnode_link *melnode_link_get(u8 peer_id);
void melnode_link_put(struct melnode_link *link);

int melnode_routing_send_now(struct melnode_link *link, const u8 *plain, size_t plain_len);
int melnode_routing_route_and_send(u8 dst_peer_id, u8 *plain, size_t plain_len);
void melnode_routing_deliver_or_forward(u8 src, u8 dst, u8 ttl, u8 *plain, size_t plain_len);

void melnode_send_initiation(struct melnode_link *link);

#endif /* _MELNODE_ROUTING_H */
