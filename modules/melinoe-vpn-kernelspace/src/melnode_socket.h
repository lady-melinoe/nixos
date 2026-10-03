/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_SOCKET_H
#define _MELNODE_SOCKET_H

#include <linux/types.h>
#include <linux/socket.h>

struct melnode_instance;
struct melnode_endpoint;

void melnode_socket_init_work(struct melnode_instance *inst);
int melnode_socket_open(struct melnode_instance *inst, u16 local_port, u32 fwmark);
void melnode_socket_close(struct melnode_instance *inst);
int melnode_socket_send(struct melnode_instance *inst, const void *buf, size_t len,
			const struct melnode_endpoint *ep, u8 tos);

#endif
