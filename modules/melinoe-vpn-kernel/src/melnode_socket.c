// SPDX-License-Identifier: GPL-2.0-only
/*
 * melnode's UDP socket - see melnode_socket.h for the design (plain kernel
 * sockets + a deferred workqueue instead of WireGuard's udp_tunnel/dst-cache
 * machinery).
 */

#include <linux/net.h>
#include <linux/inet.h>
#include <linux/in.h>
#include <linux/in6.h>
#include <net/sock.h>
#include <net/net_namespace.h>

#include "melnode_core.h"
#include "melnode_socket.h"

/* One recvmsg buffer, reused for the whole rx_work run - protected by the
 * fact that rx_work is a single, non-reentrant work item (see
 * melnode_socket_open()'s use of the default system_wq semantics: a given
 * work_struct never runs concurrently with itself).
 */
#define MELNODE_RECV_BUF_SIZE 65535

static void melnode_data_ready4(struct sock *sk)
{
	struct melnode_device *dev = sk->sk_user_data;

	if (dev->orig_sk_data_ready4)
		dev->orig_sk_data_ready4(sk);
	schedule_work(&dev->rx_work);
}

static void melnode_data_ready6(struct sock *sk)
{
	struct melnode_device *dev = sk->sk_user_data;

	if (dev->orig_sk_data_ready6)
		dev->orig_sk_data_ready6(sk);
	schedule_work(&dev->rx_work);
}

static int open_one(struct melnode_device *dev, int family, u16 local_port, struct socket **sockp)
{
	struct sockaddr_storage addr = { 0 };
	int addr_len;
	int err;

	err = sock_create_kern(&init_net, family, SOCK_DGRAM, IPPROTO_UDP, sockp);
	if (err)
		return err;

	if (family == AF_INET) {
		struct sockaddr_in *a4 = (struct sockaddr_in *)&addr;

		a4->sin_family = AF_INET;
		a4->sin_addr.s_addr = htonl(INADDR_ANY);
		a4->sin_port = htons(local_port);
		addr_len = sizeof(*a4);
	} else {
		struct sockaddr_in6 *a6 = (struct sockaddr_in6 *)&addr;

		a6->sin6_family = AF_INET6;
		a6->sin6_addr = in6addr_any;
		a6->sin6_port = htons(local_port);
		addr_len = sizeof(*a6);
	}

	err = kernel_bind(*sockp, (struct sockaddr_unsized *)&addr, addr_len);
	if (err) {
		sock_release(*sockp);
		*sockp = NULL;
		return err;
	}

	lock_sock((*sockp)->sk);
	/* Unlike an earlier version of this module, nothing reaches
	 * melnode_socket_send() from softirq context any more: the routing
	 * queue (melnode_routing.c) means melnode_tun_xmit() only ever does
	 * a cheap RCU lookup and an
	 * enqueue, never touches the socket - the actual send only ever
	 * happens from a link's own outq_work or from INJECT's genl doit,
	 * both plain process context. So sk_allocation stays at its default
	 * (GFP_KERNEL, sleepable) rather than being forced to GFP_ATOMIC.
	 */
	(*sockp)->sk->sk_user_data = dev;
	if (family == AF_INET) {
		dev->orig_sk_data_ready4 = (*sockp)->sk->sk_data_ready;
		(*sockp)->sk->sk_data_ready = melnode_data_ready4;
	} else {
		dev->orig_sk_data_ready6 = (*sockp)->sk->sk_data_ready;
		(*sockp)->sk->sk_data_ready = melnode_data_ready6;
	}
	release_sock((*sockp)->sk);

	return 0;
}

int melnode_socket_open(struct melnode_device *dev, u16 local_port)
{
	int err;

	err = open_one(dev, AF_INET, local_port, &dev->sock4);
	if (err)
		return err;

	err = open_one(dev, AF_INET6, local_port, &dev->sock6);
	if (err) {
		/* IPv6 may simply be unavailable (CONFIG_IPV6=n, or
		 * EAFNOSUPPORT) - melnode still works over v4 only.
		 */
		dev->sock6 = NULL;
	}

	return 0;
}

void melnode_socket_close(struct melnode_device *dev)
{
	if (dev->sock4) {
		sock_release(dev->sock4);
		dev->sock4 = NULL;
	}
	if (dev->sock6) {
		sock_release(dev->sock6);
		dev->sock6 = NULL;
	}
	cancel_work_sync(&dev->rx_work);
}

int melnode_socket_send(struct melnode_device *dev, const void *buf, size_t len,
			 const void *addr, int addr_len)
{
	const struct sockaddr *sa = addr;
	struct socket *sock;
	struct msghdr msg = { 0 };
	struct kvec iov;

	if (sa->sa_family == AF_INET)
		sock = dev->sock4;
	else if (sa->sa_family == AF_INET6)
		sock = dev->sock6;
	else
		return -EAFNOSUPPORT;

	if (!sock)
		return -ENETUNREACH;

	iov.iov_base = (void *)buf;
	iov.iov_len = len;
	msg.msg_name = (void *)addr;
	msg.msg_namelen = addr_len;
	/* Every caller now runs from process context (see open_one()'s
	 * comment above), so blocking here would no longer risk a
	 * scheduling-from-atomic-context bug the way it once did. Kept
	 * anyway, on purpose: without it, a full sk_sndbuf would put a link's
	 * own outq_work to sleep in sock_alloc_send_pskb() - safe, but ties
	 * up a shared workqueue worker that could otherwise be draining a
	 * *different* link's queue. Failing fast with -EAGAIN instead matches
	 * melnode's existing "no staged-packet queue" design (see
	 * melnode_core.c's file header) - callers already treat a failed
	 * send as a dropped packet.
	 */
	msg.msg_flags = MSG_DONTWAIT;

	return kernel_sendmsg(sock, &msg, &iov, 1, len);
}

/* Drains one socket's receive queue (MSG_DONTWAIT), dispatching each
 * datagram to melnode_handle_datagram(). Runs in process context (this is
 * rx_work's body, scheduled by melnode_data_ready{4,6} above).
 */
static void drain_socket(struct melnode_device *dev, struct socket *sock, u8 *buf)
{
	struct sockaddr_storage from;
	struct msghdr msg = { 0 };
	struct kvec iov;
	int ret;

	if (!sock)
		return;

	for (;;) {
		iov.iov_base = buf;
		iov.iov_len = MELNODE_RECV_BUF_SIZE;
		msg.msg_name = &from;
		msg.msg_namelen = sizeof(from);

		ret = kernel_recvmsg(sock, &msg, &iov, 1, MELNODE_RECV_BUF_SIZE, MSG_DONTWAIT);
		if (ret <= 0)
			break;

		melnode_handle_datagram(buf, ret, &from, msg.msg_namelen);
	}
}

static void melnode_rx_work_fn(struct work_struct *work)
{
	struct melnode_device *dev = container_of(work, struct melnode_device, rx_work);
	u8 *buf;

	buf = kmalloc(MELNODE_RECV_BUF_SIZE, GFP_KERNEL);
	if (!buf)
		return;

	drain_socket(dev, dev->sock4, buf);
	drain_socket(dev, dev->sock6, buf);

	kfree(buf);
}

/* melnode_core.c wires this up at module init (INIT_WORK needs the function
 * pointer at compile time, but the work_struct itself lives on
 * melnode_dev, defined there) - see melnode_socket_init_work().
 */
void melnode_socket_init_work(struct melnode_device *dev)
{
	INIT_WORK(&dev->rx_work, melnode_rx_work_fn);
}
