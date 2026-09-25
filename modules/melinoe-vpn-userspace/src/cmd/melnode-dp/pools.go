/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package main

import (
	"sync"
)

// WaitPool is a free list, tuned for two independent concerns:
//
//   - A hard cap on outstanding (Get'd but not yet Put back) objects,
//     via max -- 0 disables this entirely (Get never blocks). This is
//     the only thing the original (sync.Pool-backed) version of this
//     type did.
//   - A genuinely pre-filled reuse pool, immune to GC -- added because
//     a bare sync.Pool doesn't actually solve cold-start cost the way
//     it looks like it should: sync.Pool aggressively evicts its
//     contents across GC cycles (that's how it keeps memory use low
//     when idle), so after any idle period long enough for even one GC
//     to run, the *next* burst of traffic pays full fresh-allocation
//     cost for every object until enough have cycled through Put to
//     refill it again. For device.pool.messageBuffers specifically,
//     each object is a 64KB array (MaxMessageSize) -- repeatedly
//     allocating and zeroing those at the start of a burst is real,
//     measurable cost, and was reproduced directly as a multi-second
//     throughput ramp-up on a fresh high-rate UDP flow (see
//     PROJECT_STATE.md). A `chan any` free list doesn't have this
//     problem: an object sitting in a channel's buffer is an ordinary
//     live reference as far as the garbage collector is concerned, not
//     a victim-cache entry, so PoolPrefillSize objects seeded at
//     startup (NewWaitPool) stay ready indefinitely, no matter how long
//     the gap before the first real burst.
type WaitPool struct {
	free  chan any
	new   func() any
	max   uint32
	mu    sync.Mutex
	cond  sync.Cond
	count uint32 // outstanding (Get'd, not yet Put) -- only tracked/used when max != 0
}

func NewWaitPool(max uint32, new func() any) *WaitPool {
	p := &WaitPool{new: new, max: max}
	p.cond = sync.Cond{L: &p.mu}
	cap := PoolPrefillSize
	if cap == 0 {
		cap = 1 // a zero-capacity channel can never hold anything, degrading to alloc-every-time; always keep at least a minimal free list
	}
	p.free = make(chan any, cap)
	for i := 0; i < PoolPrefillSize; i++ {
		p.free <- new()
	}
	return p
}

func (p *WaitPool) Get() any {
	if p.max != 0 {
		p.mu.Lock()
		for p.count >= p.max {
			p.cond.Wait()
		}
		p.count++
		p.mu.Unlock()
	}
	select {
	case x := <-p.free:
		return x
	default:
		return p.new()
	}
}

func (p *WaitPool) Put(x any) {
	select {
	case p.free <- x:
	default:
		// Free list already at capacity -- drop x and let the GC
		// reclaim it rather than growing the list further. Only
		// reachable when more objects are ever outstanding at once
		// than PoolPrefillSize, i.e. this is a load spike bigger than
		// what was pre-provisioned for, not a bug.
	}
	if p.max == 0 {
		return
	}
	p.mu.Lock()
	p.count--
	p.cond.Signal()
	p.mu.Unlock()
}

func (device *Device) PopulatePools() {
	device.pool.inboundElementsContainer = NewWaitPool(PreallocatedBuffersPerPool, func() any {
		s := make([]*QueueInboundElement, 0, device.BatchSize())
		return &QueueInboundElementsContainer{elems: s}
	})
	device.pool.outboundElementsContainer = NewWaitPool(PreallocatedBuffersPerPool, func() any {
		s := make([]*QueueOutboundElement, 0, device.BatchSize())
		return &QueueOutboundElementsContainer{elems: s}
	})
	device.pool.messageBuffers = NewWaitPool(PreallocatedBuffersPerPool, func() any {
		return new([MaxMessageSize]byte)
	})
	device.pool.inboundElements = NewWaitPool(PreallocatedBuffersPerPool, func() any {
		return new(QueueInboundElement)
	})
	device.pool.outboundElements = NewWaitPool(PreallocatedBuffersPerPool, func() any {
		return new(QueueOutboundElement)
	})
}

func (device *Device) GetInboundElementsContainer() *QueueInboundElementsContainer {
	c := device.pool.inboundElementsContainer.Get().(*QueueInboundElementsContainer)
	c.Mutex = sync.Mutex{}
	return c
}

func (device *Device) PutInboundElementsContainer(c *QueueInboundElementsContainer) {
	for i := range c.elems {
		c.elems[i] = nil
	}
	c.elems = c.elems[:0]
	device.pool.inboundElementsContainer.Put(c)
}

func (device *Device) GetOutboundElementsContainer() *QueueOutboundElementsContainer {
	c := device.pool.outboundElementsContainer.Get().(*QueueOutboundElementsContainer)
	c.Mutex = sync.Mutex{}
	c.isControl = false
	c.forwarded = false
	return c
}

func (device *Device) PutOutboundElementsContainer(c *QueueOutboundElementsContainer) {
	for i := range c.elems {
		c.elems[i] = nil
	}
	c.elems = c.elems[:0]
	device.pool.outboundElementsContainer.Put(c)
}

func (device *Device) GetMessageBuffer() *[MaxMessageSize]byte {
	return device.pool.messageBuffers.Get().(*[MaxMessageSize]byte)
}

func (device *Device) PutMessageBuffer(msg *[MaxMessageSize]byte) {
	device.pool.messageBuffers.Put(msg)
}

func (device *Device) GetInboundElement() *QueueInboundElement {
	return device.pool.inboundElements.Get().(*QueueInboundElement)
}

func (device *Device) PutInboundElement(elem *QueueInboundElement) {
	elem.clearPointers()
	device.pool.inboundElements.Put(elem)
}

func (device *Device) GetOutboundElement() *QueueOutboundElement {
	return device.pool.outboundElements.Get().(*QueueOutboundElement)
}

func (device *Device) PutOutboundElement(elem *QueueOutboundElement) {
	elem.clearPointers()
	device.pool.outboundElements.Put(elem)
}
