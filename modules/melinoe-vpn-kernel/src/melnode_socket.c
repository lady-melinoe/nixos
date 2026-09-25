// SPDX-License-Identifier: GPL-2.0-only

#include <linux/net.h>
#include <linux/inet.h>
#include <linux/in.h>
#include <linux/in6.h>
#include <linux/ipv6.h>
#include <net/sock.h>
#include <net/net_namespace.h>

#include "melnode_compat.h"
#include "melnode_core.h"
#include "melnode_socket.h"

#define MELNODE_RECV_BUF_SIZE 65535

union melnode_cmsg_buf {
	u8 raw[CMSG_SPACE(sizeof(struct in6_pktinfo)) + CMSG_SPACE(sizeof(int))];
	struct cmsghdr align;
};

static void put_cmsg_raw(struct msghdr *msg, u8 *buf, int level, int type, const void *data,
			 size_t size)
{
	struct cmsghdr *cmsg = (struct cmsghdr *)(buf + msg->msg_controllen);

	cmsg->cmsg_level = level;
	cmsg->cmsg_type = type;
	cmsg->cmsg_len = CMSG_LEN(size);
	memcpy(CMSG_DATA(cmsg), data, size);
	msg->msg_control = buf;
	msg->msg_controllen += CMSG_SPACE(size);
}

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
	struct sockaddr_storage addr = {};
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

		melnode_compat_setsockopt_int(sock, IPPROTO_IP, IP_PKTINFO, 1);
		melnode_compat_setsockopt_int(sock, IPPROTO_IP, IP_MTU_DISCOVER, IP_PMTUDISC_DONT);
		a4->sin_family = AF_INET;
		a4->sin_addr.s_addr = htonl(INADDR_ANY);
		a4->sin_port = htons(local_port);
		addr_len = sizeof(*a4);
	} else {
		struct sockaddr_in6 *a6 = (struct sockaddr_in6 *)&addr;

		melnode_compat_sock_set_v6only(sk);
		melnode_compat_setsockopt_int(sock, IPPROTO_IPV6, IPV6_RECVPKTINFO, 1);
		melnode_compat_setsockopt_int(sock, IPPROTO_IPV6, IPV6_MTU_DISCOVER,
					      IPV6_PMTUDISC_DONT);
		a6->sin6_family = AF_INET6;
		a6->sin6_addr = in6addr_any;
		a6->sin6_port = htons(local_port);
		addr_len = sizeof(*a6);
	}

	err = melnode_compat_bind(sock, &addr, addr_len);
	if (err) {
		sock_release(sock);
		return err;
	}

	lock_sock(sk);
	melnode_compat_sock_set_mark(sk, fwmark);
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
	struct socket *sock4, *sock6;

	down_write(&dev->sock_sem);
	sock4 = dev->sock4;
	sock6 = dev->sock6;
	WRITE_ONCE(dev->sock4, NULL);
	WRITE_ONCE(dev->sock6, NULL);
	up_write(&dev->sock_sem);

	if (sock4)
		detach_socket(sock4, dev->orig_sk_data_ready4);
	if (sock6)
		detach_socket(sock6, dev->orig_sk_data_ready6);

	synchronize_rcu();
	cancel_work_sync(&dev->rx_work);

	if (sock4)
		sock_release(sock4);
	if (sock6)
		sock_release(sock6);
}

int melnode_socket_send(struct melnode_device *dev, const void *buf, size_t len,
			const struct melnode_endpoint *ep, u8 tos)
{
	const struct sockaddr *sa = (const struct sockaddr *)ep->addr;
	union melnode_cmsg_buf cbuf = {};
	struct msghdr msg = {};
	struct socket *sock;
	struct kvec iov;
	int ret;

	if (sa->sa_family != AF_INET && sa->sa_family != AF_INET6)
		return -EAFNOSUPPORT;

	iov.iov_base = (void *)buf;
	iov.iov_len = len;
	msg.msg_name = (void *)ep->addr;
	msg.msg_namelen = ep->addr_len;
	msg.msg_flags = MSG_DONTWAIT;

	if (sa->sa_family == AF_INET) {
		if (ep->has_src) {
			struct in_pktinfo pi = { .ipi_ifindex = ep->src_ifindex,
						 .ipi_spec_dst.s_addr = ep->src.v4 };

			put_cmsg_raw(&msg, cbuf.raw, IPPROTO_IP, IP_PKTINFO, &pi, sizeof(pi));
		}
		if (tos) {
			int val = tos;

			put_cmsg_raw(&msg, cbuf.raw, IPPROTO_IP, IP_TOS, &val, sizeof(val));
		}
	} else {
		if (ep->has_src) {
			struct in6_pktinfo pi = { .ipi6_addr = ep->src.v6,
						  .ipi6_ifindex = ep->src_ifindex };

			put_cmsg_raw(&msg, cbuf.raw, IPPROTO_IPV6, IPV6_PKTINFO, &pi, sizeof(pi));
		}
		if (tos) {
			int val = tos;

			put_cmsg_raw(&msg, cbuf.raw, IPPROTO_IPV6, IPV6_TCLASS, &val, sizeof(val));
		}
	}

	down_read(&dev->sock_sem);
	sock = sa->sa_family == AF_INET ? dev->sock4 : dev->sock6;
	ret = sock ? kernel_sendmsg(sock, &msg, &iov, 1, len) : -ENETUNREACH;
	up_read(&dev->sock_sem);
	return ret;
}

static void parse_pktinfo(const u8 *buf, size_t len, struct melnode_endpoint *ep)
{
	size_t off = 0;

	while (len - off >= sizeof(struct cmsghdr)) {
		const struct cmsghdr *cmsg = (const struct cmsghdr *)(buf + off);

		if (cmsg->cmsg_len < sizeof(*cmsg) || cmsg->cmsg_len > len - off)
			break;

		if (cmsg->cmsg_level == IPPROTO_IP && cmsg->cmsg_type == IP_PKTINFO &&
		    cmsg->cmsg_len >= CMSG_LEN(sizeof(struct in_pktinfo))) {
			const struct in_pktinfo *pi = (const struct in_pktinfo *)CMSG_DATA(cmsg);

			ep->src.v4 = pi->ipi_addr.s_addr;
			ep->src_ifindex = pi->ipi_ifindex;
			ep->has_src = true;
		} else if (cmsg->cmsg_level == IPPROTO_IPV6 && cmsg->cmsg_type == IPV6_PKTINFO &&
			   cmsg->cmsg_len >= CMSG_LEN(sizeof(struct in6_pktinfo))) {
			const struct in6_pktinfo *pi = (const struct in6_pktinfo *)CMSG_DATA(cmsg);

			ep->src.v6 = pi->ipi6_addr;
			ep->src_ifindex = pi->ipi6_ifindex;
			ep->has_src = true;
		}
		off += CMSG_ALIGN(cmsg->cmsg_len);
	}
}

static void drain_socket(struct socket *sock, u8 *buf)
{
	struct sockaddr_storage from;
	union melnode_cmsg_buf cbuf;
	struct melnode_endpoint ep;
	struct msghdr msg;
	struct kvec iov;
	int ret;

	if (!sock)
		return;

	for (;;) {
		memset(&msg, 0, sizeof(msg));
		iov.iov_base = buf;
		iov.iov_len = MELNODE_RECV_BUF_SIZE;
		msg.msg_name = &from;
		msg.msg_namelen = sizeof(from);
		memset(&cbuf, 0, sizeof(cbuf));
		msg.msg_control = cbuf.raw;
		msg.msg_controllen = sizeof(cbuf.raw);

		ret = kernel_recvmsg(sock, &msg, &iov, 1, MELNODE_RECV_BUF_SIZE, MSG_DONTWAIT);
		if (ret <= 0)
			break;

		if (msg.msg_namelen < sizeof(struct sockaddr_in) ||
		    msg.msg_namelen > sizeof(struct sockaddr_in6))
			continue;

		memset(&ep, 0, sizeof(ep));
		memcpy(ep.addr, &from, msg.msg_namelen);
		ep.addr_len = msg.msg_namelen;
		parse_pktinfo(cbuf.raw, sizeof(cbuf.raw) - msg.msg_controllen, &ep);

		melnode_handle_datagram(buf, ret, &ep);
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
