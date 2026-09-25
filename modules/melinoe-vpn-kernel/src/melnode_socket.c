// SPDX-License-Identifier: GPL-2.0-only

#include <linux/net.h>
#include <linux/inet.h>
#include <linux/in.h>
#include <linux/in6.h>
#include <net/sock.h>
#include <net/net_namespace.h>

#include "melnode_core.h"
#include "melnode_socket.h"

#define MELNODE_RECV_BUF_SIZE 65535

static void melnode_data_ready4(struct sock *sk)
{
	struct melnode_device *dev = sk->sk_user_data;

	if (!dev)
		return;
	if (dev->orig_sk_data_ready4)
		dev->orig_sk_data_ready4(sk);
	schedule_work(&dev->rx_work);
}

static void melnode_data_ready6(struct sock *sk)
{
	struct melnode_device *dev = sk->sk_user_data;

	if (!dev)
		return;
	if (dev->orig_sk_data_ready6)
		dev->orig_sk_data_ready6(sk);
	schedule_work(&dev->rx_work);
}

static int open_one(struct melnode_device *dev, int family, u16 local_port, u32 fwmark,
		    struct socket **sockp)
{
	struct sockaddr_storage addr = { 0 };
	struct socket *sock;
	struct sock *sk;
	int addr_len;
	int err;

	err = sock_create_kern(&init_net, family, SOCK_DGRAM, IPPROTO_UDP, &sock);
	if (err)
		return err;
	sk = sock->sk;

	if (family == AF_INET) {
		struct sockaddr_in *a4 = (struct sockaddr_in *)&addr;

		a4->sin_family = AF_INET;
		a4->sin_addr.s_addr = htonl(INADDR_ANY);
		a4->sin_port = htons(local_port);
		addr_len = sizeof(*a4);
	} else {
		struct sockaddr_in6 *a6 = (struct sockaddr_in6 *)&addr;

		sk->sk_ipv6only = 1;
		a6->sin6_family = AF_INET6;
		a6->sin6_addr = in6addr_any;
		a6->sin6_port = htons(local_port);
		addr_len = sizeof(*a6);
	}

	err = kernel_bind(sock, (struct sockaddr_unsized *)&addr, addr_len);
	if (err) {
		sock_release(sock);
		return err;
	}

	lock_sock(sk);
	WRITE_ONCE(sk->sk_mark, fwmark);
	sk->sk_user_data = dev;
	if (family == AF_INET) {
		dev->orig_sk_data_ready4 = sk->sk_data_ready;
		sk->sk_data_ready = melnode_data_ready4;
	} else {
		dev->orig_sk_data_ready6 = sk->sk_data_ready;
		sk->sk_data_ready = melnode_data_ready6;
	}
	release_sock(sk);

	*sockp = sock;
	return 0;
}

int melnode_socket_open(struct melnode_device *dev, u16 local_port, u32 fwmark)
{
	int err;

	err = open_one(dev, AF_INET, local_port, fwmark, &dev->sock4);
	if (err)
		return err;

	if (open_one(dev, AF_INET6, local_port, fwmark, &dev->sock6))
		dev->sock6 = NULL;

	return 0;
}

static void detach_socket(struct socket *sock, void (*orig_data_ready)(struct sock *))
{
	struct sock *sk = sock->sk;

	write_lock_bh(&sk->sk_callback_lock);
	sk->sk_data_ready = orig_data_ready;
	sk->sk_user_data = NULL;
	write_unlock_bh(&sk->sk_callback_lock);
}

void melnode_socket_close(struct melnode_device *dev)
{
	struct socket *sock4 = dev->sock4;
	struct socket *sock6 = dev->sock6;

	WRITE_ONCE(dev->sock4, NULL);
	WRITE_ONCE(dev->sock6, NULL);

	if (sock4)
		detach_socket(sock4, dev->orig_sk_data_ready4);
	if (sock6)
		detach_socket(sock6, dev->orig_sk_data_ready6);

	cancel_work_sync(&dev->rx_work);

	if (sock4)
		sock_release(sock4);
	if (sock6)
		sock_release(sock6);
}

int melnode_socket_send(struct melnode_device *dev, const void *buf, size_t len, const void *addr,
			int addr_len)
{
	const struct sockaddr *sa = addr;
	struct msghdr msg = { 0 };
	struct socket *sock;
	struct kvec iov;

	if (sa->sa_family == AF_INET)
		sock = READ_ONCE(dev->sock4);
	else if (sa->sa_family == AF_INET6)
		sock = READ_ONCE(dev->sock6);
	else
		return -EAFNOSUPPORT;

	if (!sock)
		return -ENETUNREACH;

	iov.iov_base = (void *)buf;
	iov.iov_len = len;
	msg.msg_name = (void *)addr;
	msg.msg_namelen = addr_len;
	msg.msg_flags = MSG_DONTWAIT;

	return kernel_sendmsg(sock, &msg, &iov, 1, len);
}

static void drain_socket(struct socket *sock, u8 *buf)
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
		cond_resched();
	}
}

static void melnode_rx_work_fn(struct work_struct *work)
{
	struct melnode_device *dev = container_of(work, struct melnode_device, rx_work);
	u8 *buf;

	buf = kmalloc(MELNODE_RECV_BUF_SIZE, GFP_KERNEL);
	if (!buf)
		return;

	drain_socket(READ_ONCE(dev->sock4), buf);
	drain_socket(READ_ONCE(dev->sock6), buf);

	kfree(buf);
}

void melnode_socket_init_work(struct melnode_device *dev)
{
	INIT_WORK(&dev->rx_work, melnode_rx_work_fn);
}
