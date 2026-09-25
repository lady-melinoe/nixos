/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * The UDP socket melnode's data plane listens and sends on.
 *
 * Simplified from drivers/net/wireguard/socket.c (Jason A. Donenfeld,
 * GPL-2.0): WireGuard uses net/udp_tunnel.h's encap_rcv/dst-caching
 * machinery for zero-copy, route-cached sends. This instead uses plain
 * kernel sockets (sock_create_kern + kernel_bind) with sk_data_ready
 * overridden to schedule a workqueue item - simpler to get right, at the
 * cost of a route lookup per send instead of a cached one. Same "simple
 * now, revisit if it matters" reasoning as melnode_transport.h.
 *
 * sk_data_ready fires in softirq context, where taking a sleeping lock or
 * doing GFP_KERNEL crypto work is not allowed - it only schedules
 * melnode_dev's rx_work; the actual recvmsg + dispatch loop runs in that
 * work's process context.
 */
#ifndef _MELNODE_SOCKET_H
#define _MELNODE_SOCKET_H

#include <linux/types.h>
#include <linux/socket.h>
#include <net/net_namespace.h>

struct melnode_device;

/* Binds both an IPv4 and (if available) an IPv6 socket to local_port in
 * init_net, marked with fwmark (SO_MARK-equivalent, sk->sk_mark) so host
 * policy routing can steer this socket's own traffic away from whatever
 * the overlay itself is routing - without this, an advertised overlay
 * prefix that happens to cover a peer's real endpoint address routes the
 * transport's own encrypted packets back into a tun instead of out the
 * real interface, which the tun then re-wraps and sends again: the tunnel
 * forwarding into itself, unbounded. melnode-dp (userspace) already does
 * this via bind.SetMark(); the kernel module must match it. On success,
 * melnode_dev.sock4/sock6 are set and rx_work will fire on incoming
 * datagrams; call melnode_socket_close() to tear down.
 */
/* Must be called once (module init, before any DEVICE_SET) to set up
 * dev->rx_work - separate from melnode_socket_open() since the work_struct
 * itself lives for the module's lifetime, not just while a socket is open.
 */
void melnode_socket_init_work(struct melnode_device *dev);

int melnode_socket_open(struct melnode_device *dev, u16 local_port, u32 fwmark);
void melnode_socket_close(struct melnode_device *dev);

/* Sends len bytes to the given sockaddr (a struct sockaddr_in or
 * sockaddr_in6, addr_len says which). Best-effort: errors are the caller's
 * to count, not retry.
 */
int melnode_socket_send(struct melnode_device *dev, const void *buf, size_t len,
			 const void *addr, int addr_len);

#endif /* _MELNODE_SOCKET_H */
