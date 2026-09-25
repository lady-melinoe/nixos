/*
 * melnode_genl.h -- generic netlink API between melnode's control plane and
 * its data plane. The userspace data plane (cmd/melnode-dp) implements this
 * exactly, over a unix seqpacket socket instead of AF_NETLINK; a kernel
 * module implements it as a genl family, and melnode-cp needs no changes.
 *
 * API.md has the semantics. The numbers here are checked against the Go
 * implementation (dpproto) by a test, so they cannot drift apart.
 *
 * No licence header on purpose: which one this file carries is for the
 * kernel module's author to decide.
 */
#ifndef _MELNODE_GENL_H
#define _MELNODE_GENL_H

#define MELNODE_GENL_NAME	"melnode"
#define MELNODE_GENL_VERSION	3

enum melnode_genl_cmd {
	MELNODE_CMD_UNSPEC = 0,
	MELNODE_CMD_HELLO = 1,		/* doit */
	MELNODE_CMD_ATTACH = 2,		/* doit */
	MELNODE_CMD_DEVICE_SET = 3,	/* doit */
	MELNODE_CMD_DEVICE_DEL = 4,	/* doit */
	MELNODE_CMD_STATS_GET = 5,	/* dump */
	MELNODE_CMD_LINK_ADD = 6,	/* doit */
	MELNODE_CMD_LINK_DEL = 7,	/* doit */
	MELNODE_CMD_LINK_GET = 8,	/* dump */
	MELNODE_CMD_TUN_CREATE = 9,	/* doit */
	MELNODE_CMD_TUN_START = 10,	/* doit */
	MELNODE_CMD_TUN_DESTROY = 11,	/* doit */
	MELNODE_CMD_TUN_GET = 12,	/* dump */
	MELNODE_CMD_ROUTE_SET = 13,	/* doit */
	MELNODE_CMD_ROUTE_DEL = 14,	/* doit */
	MELNODE_CMD_ROUTE_GET = 15,	/* dump */
	MELNODE_CMD_PUNT = 16,		/* data plane -> control plane */
	MELNODE_CMD_INJECT = 17,	/* control plane -> data plane */
	MELNODE_CMD_EVENT = 18,		/* data plane -> control plane, mcast */

	/* Vendor range: the userspace data plane only. Nothing else may need it. */
	MELNODE_CMD_X_QUIT = 128,
};

enum melnode_genl_attr {
	MELNODE_A_UNSPEC = 0,
	MELNODE_A_PAD = 1,		/* alignment before u64 attributes */
	MELNODE_A_API_VERSION = 2,	/* NLA_U32 */
	MELNODE_A_PID = 3,		/* NLA_U32, userspace data plane only */
	MELNODE_A_CONFIGURED = 4,	/* NLA_U8 */
	MELNODE_A_LOCAL_ID = 5,		/* NLA_U32, 0..255 */
	MELNODE_A_PRIVATE_KEY = 6,	/* NLA_BINARY, exactly 32 bytes */
	MELNODE_A_PUBLIC_KEY = 7,	/* NLA_BINARY, exactly 32 bytes */
	MELNODE_A_LISTEN_PORT = 8,	/* NLA_U16, host order */
	MELNODE_A_FWMARK = 9,		/* NLA_U32 */
	MELNODE_A_MTU = 10,		/* NLA_U32, 576..65000 */
	MELNODE_A_PEER_ID = 11,		/* NLA_U32, 0..255 */
	MELNODE_A_ENDPOINT = 12,	/* NLA_BINARY: struct sockaddr_in / _in6 */
	MELNODE_A_LAST_HANDSHAKE = 13,	/* NLA_U64, unix ns, 0 = never */
	MELNODE_A_TX_BYTES = 14,	/* NLA_U64 */
	MELNODE_A_RX_BYTES = 15,	/* NLA_U64 */
	MELNODE_A_TUN_NAME = 16,	/* NLA_NUL_STRING, < IFNAMSIZ */
	MELNODE_A_TUN_STARTED = 17,	/* NLA_U8 */
	MELNODE_A_ROUTE_DST = 18,	/* NLA_U32, 0..255 */
	MELNODE_A_ROUTE_NEXTHOP = 19,	/* NLA_U32, 0..255 */
	MELNODE_A_STAT_ID = 20,		/* NLA_U16 */
	MELNODE_A_STAT_VALUE = 21,	/* NLA_U64 */
	MELNODE_A_PKT_LINK = 22,	/* NLA_U32 */
	MELNODE_A_PKT_PROTO = 23,	/* NLA_U8 */
	MELNODE_A_PKT_SRC = 24,		/* NLA_U8 */
	MELNODE_A_PKT_DST = 25,		/* NLA_U8 */
	MELNODE_A_PKT_TTL = 26,		/* NLA_U8 */
	MELNODE_A_PKT_DATA = 27,	/* NLA_BINARY */
	MELNODE_A_EVT_KIND = 28,	/* NLA_U8 */
	MELNODE_A_EVT_TIME = 29,	/* NLA_U64, unix ns */
};

/* Errors are errnos: EINVAL, ENOENT, EEXIST, EIO, EOPNOTSUPP, ENODEV. */

enum melnode_event_kind {
	MELNODE_EVENT_LINK_HANDSHAKE = 1,
};

enum melnode_stat {
	MELNODE_STAT_PUNT_SENT = 1,
	MELNODE_STAT_PUNT_DROPPED = 2,
	MELNODE_STAT_INJECT_SENT = 3,
	MELNODE_STAT_INJECT_DROPPED = 4,
	MELNODE_STAT_RX_NO_ROUTE = 5,
	MELNODE_STAT_RX_TTL_EXPIRED = 6,
	MELNODE_STAT_RX_NO_TUN = 7,
	MELNODE_STAT_RX_TUN_FULL = 8,
	MELNODE_STAT_RX_BAD_PACKET = 9,
	MELNODE_STAT_TX_NO_ROUTE = 10,
	MELNODE_STAT_TX_QUEUE_FULL = 11,
	MELNODE_STAT_EVENTS_DROPPED = 12,
	MELNODE_STAT_RX_QUEUE_FULL = 13,
};

#endif /* _MELNODE_GENL_H */
