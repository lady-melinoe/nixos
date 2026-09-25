/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef _MELNODE_COMPAT_H
#define _MELNODE_COMPAT_H

#include <linux/net.h>
#include <linux/netdevice.h>
#include <linux/skbuff.h>
#include <linux/unaligned.h>
#include <linux/sockptr.h>
#include <net/genetlink.h>
#include <net/gro_cells.h>
#include <net/sock.h>
#include <crypto/blake2s.h>

typedef struct blake2s_ctx melnode_blake2s_ctx;

static inline int melnode_compat_bind(struct socket *sock, struct sockaddr_storage *addr,
				      int addr_len)
{
	return kernel_bind(sock, (struct sockaddr_unsized *)addr, addr_len);
}

static inline void melnode_compat_sock_set_v6only(struct sock *sk)
{
	sk->sk_ipv6only = 1;
}

static inline void melnode_compat_sock_set_mark(struct sock *sk, u32 mark)
{
	WRITE_ONCE(sk->sk_mark, mark);
}

static inline int melnode_compat_gro_receive(struct gro_cells *cells, struct sk_buff *skb)
{
	return gro_cells_receive(cells, skb);
}

static inline ssize_t melnode_compat_nla_strscpy(char *dst, const struct nlattr *nla, size_t size)
{
	return nla_strscpy(dst, nla, size);
}

static inline u8 melnode_compat_genl_cmd(const struct genl_split_ops *ops)
{
	return ops->cmd;
}

static inline int melnode_compat_setsockopt_int(struct socket *sock, int level, int optname,
						int val)
{
	return sock->ops->setsockopt(sock, level, optname, KERNEL_SOCKPTR(&val), sizeof(val));
}

#endif /* _MELNODE_COMPAT_H */
