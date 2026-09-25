/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 *
 * This is based heavily on timers.c from the kernel implementation.
 */

package main

import (
	"melnode/dpproto"
	"sync"
	"time"
	_ "unsafe"
)

//go:linkname fastrandn runtime.fastrandn
func fastrandn(n uint32) uint32

type Timer struct {
	*time.Timer
	modifyingLock sync.RWMutex
	runningLock   sync.Mutex
	isPending     bool
}

func (peer *Peer) NewTimer(expirationFunction func(*Peer)) *Timer {
	timer := &Timer{}
	timer.Timer = time.AfterFunc(time.Hour, func() {
		timer.runningLock.Lock()
		defer timer.runningLock.Unlock()

		timer.modifyingLock.Lock()
		if !timer.isPending {
			timer.modifyingLock.Unlock()
			return
		}
		timer.isPending = false
		timer.modifyingLock.Unlock()

		expirationFunction(peer)
	})
	timer.Stop()
	return timer
}

func (timer *Timer) Mod(d time.Duration) {
	timer.modifyingLock.Lock()
	timer.isPending = true
	timer.Reset(d)
	timer.modifyingLock.Unlock()
}

func (timer *Timer) Del() {
	timer.modifyingLock.Lock()
	timer.isPending = false
	timer.Stop()
	timer.modifyingLock.Unlock()
}

func (timer *Timer) DelSync() {
	timer.Del()
	timer.runningLock.Lock()
	timer.Del()
	timer.runningLock.Unlock()
}

func (timer *Timer) IsPending() bool {
	timer.modifyingLock.RLock()
	defer timer.modifyingLock.RUnlock()
	return timer.isPending
}

func (peer *Peer) timersActive() bool {
	return peer.isRunning.Load() && peer.device != nil && peer.device.isUp()
}

func expiredRetransmitHandshake(peer *Peer) {
	if peer.timers.handshakeAttempts.Load() > MaxTimerHandshakes {
		if peer.timersActive() {
			peer.timers.sendKeepalive.Del()
		}

		peer.FlushStagedPackets()

		if peer.timersActive() && !peer.timers.zeroKeyMaterial.IsPending() {
			peer.timers.zeroKeyMaterial.Mod(RejectAfterTime * 3)
		}
	} else {
		peer.timers.handshakeAttempts.Add(1)

		peer.markEndpointSrcForClearing()

		peer.SendHandshakeInitiation(true)
	}
}

func expiredSendKeepalive(peer *Peer) {
	peer.SendKeepalive()
	if peer.timers.needAnotherKeepalive.Load() {
		peer.timers.needAnotherKeepalive.Store(false)
		if peer.timersActive() {
			peer.timers.sendKeepalive.Mod(KeepaliveTimeout)
		}
	}
}

func expiredNewHandshake(peer *Peer) {
	peer.markEndpointSrcForClearing()
	peer.SendHandshakeInitiation(false)
}

func expiredZeroKeyMaterial(peer *Peer) {
	peer.ZeroAndFlushAll()
}

func expiredPersistentKeepalive(peer *Peer) {
	if peer.persistentKeepaliveInterval.Load() > 0 {
		peer.SendKeepalive()
	}
}

func (peer *Peer) timersDataSent() {
	if peer.timersActive() && !peer.timers.newHandshake.IsPending() {
		peer.timers.newHandshake.Mod(KeepaliveTimeout + RekeyTimeout + time.Millisecond*time.Duration(fastrandn(RekeyTimeoutJitterMaxMs)))
	}
}

func (peer *Peer) timersDataReceived() {
	if peer.timersActive() {
		if !peer.timers.sendKeepalive.IsPending() {
			peer.timers.sendKeepalive.Mod(KeepaliveTimeout)
		} else {
			peer.timers.needAnotherKeepalive.Store(true)
		}
	}
}

func (peer *Peer) timersAnyAuthenticatedPacketSent() {
	if peer.timersActive() {
		peer.timers.sendKeepalive.Del()
	}
}

func (peer *Peer) timersAnyAuthenticatedPacketReceived() {
	if peer.timersActive() {
		peer.timers.newHandshake.Del()
	}
}

func (peer *Peer) timersHandshakeInitiated() {
	if peer.timersActive() {
		peer.timers.retransmitHandshake.Mod(RekeyTimeout + time.Millisecond*time.Duration(fastrandn(RekeyTimeoutJitterMaxMs)))
	}
}

func (peer *Peer) timersHandshakeComplete() {
	if peer.timersActive() {
		peer.timers.retransmitHandshake.Del()
	}
	peer.timers.handshakeAttempts.Store(0)
	peer.timers.sentLastMinuteHandshake.Store(false)
	now := time.Now()
	peer.lastHandshakeNano.Store(now.UnixNano())
	if ctl := peer.device.ctl; ctl != nil {
		ev := dpproto.Event{Kind: dpproto.EventLinkHandshake, PeerID: peer.id, UnixNano: now.UnixNano()}
		peer.endpoint.Lock()
		if peer.endpoint.val != nil {
			ev.Endpoint = peer.endpoint.val.DstToString()
		}
		peer.endpoint.Unlock()
		ctl.Event(ev)
	}
}

func (peer *Peer) timersSessionDerived() {
	if peer.timersActive() {
		peer.timers.zeroKeyMaterial.Mod(RejectAfterTime * 3)
	}
}

func (peer *Peer) timersAnyAuthenticatedPacketTraversal() {
	keepalive := peer.persistentKeepaliveInterval.Load()
	if keepalive > 0 && peer.timersActive() {
		peer.timers.persistentKeepalive.Mod(time.Duration(keepalive) * time.Second)
	}
}

func (peer *Peer) timersInit() {
	peer.timers.retransmitHandshake = peer.NewTimer(expiredRetransmitHandshake)
	peer.timers.sendKeepalive = peer.NewTimer(expiredSendKeepalive)
	peer.timers.newHandshake = peer.NewTimer(expiredNewHandshake)
	peer.timers.zeroKeyMaterial = peer.NewTimer(expiredZeroKeyMaterial)
	peer.timers.persistentKeepalive = peer.NewTimer(expiredPersistentKeepalive)
}

func (peer *Peer) timersStart() {
	peer.timers.handshakeAttempts.Store(0)
	peer.timers.sentLastMinuteHandshake.Store(false)
	peer.timers.needAnotherKeepalive.Store(false)
}

func (peer *Peer) timersStop() {
	peer.timers.retransmitHandshake.DelSync()
	peer.timers.sendKeepalive.DelSync()
	peer.timers.newHandshake.DelSync()
	peer.timers.zeroKeyMaterial.DelSync()
	peer.timers.persistentKeepalive.DelSync()
}
