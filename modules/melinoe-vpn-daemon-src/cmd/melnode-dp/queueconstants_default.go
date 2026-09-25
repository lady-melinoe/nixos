//go:build !android && !ios && !windows

/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package main

import "golang.zx2c4.com/wireguard/conn"

const (
	QueueStagedSize            = conn.IdealBatchSize
	QueueOutboundSize          = 1024
	QueueInboundSize           = 1024
	QueueHandshakeSize         = 1024
	MaxSegmentSize             = (1 << 16) - 1 // largest possible UDP datagram
	PreallocatedBuffersPerPool = 0             // Disable and allow for infinite memory growth

	// maxDataInFlight caps one peer's data packets between nonce assignment
	// and the wire. Control packets are sent ahead of queued data but take
	// their nonces later, so they can overtake up to this many data packets;
	// the receiver's replay window (8192 - 64 counters, RFC-style sliding
	// window in both wireguard-go and the kernel module) rejects anything
	// further behind. Half the window leaves plenty of room for control.
	maxDataInFlight = 4096

	// PoolPrefillSize is melnode's own addition (not upstream
	// wireguard-go): how many objects each WaitPool actually allocates
	// upfront, at startup, rather than lazily on first use. Separate
	// concern from PreallocatedBuffersPerPool above -- that one bounds
	// concurrent *outstanding* objects (a hard cap, currently disabled);
	// this one is about avoiding a cold-start allocation burst the
	// first time real traffic arrives, or after any idle period long
	// enough for a GC cycle to run (see WaitPool's own doc comment in
	// pools.go for why that matters, and PROJECT_STATE.md for the
	// measured ramp-up this fixes). 1024 buffers of MaxMessageSize
	// (64KB) is 64MB pre-committed for device.pool.messageBuffers
	// specifically -- deliberate, since that's the pool actually
	// driving per-packet throughput; the other four pools hold much
	// smaller objects, so the same count costs negligible memory there.
	PoolPrefillSize = 1024
)
