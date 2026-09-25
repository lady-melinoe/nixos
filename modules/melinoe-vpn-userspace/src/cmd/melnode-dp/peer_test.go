package main

import (
	"os"
	"runtime"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/tun"
)

func testDevice(t *testing.T) *Device {
	t.Helper()
	d := idleDevice()
	d.startCryptoWorkers()
	return d
}

func idleDevice() *Device {
	var priv NoisePrivateKey
	priv[0] = 1
	d := newDevice(1, priv)
	d.router = newRouter(d, 1)
	return d
}

func testPeer(d *Device, id uint32) *Peer {
	var pk NoisePublicKey
	pk[0] = byte(id)
	p, _ := newPeer(d, id, pk, nil)
	d.addPeer(p)
	return p
}

func TestStoppedPeerIsCollectedWithoutCrash(t *testing.T) {
	d := testDevice(t)
	func() {
		p := testPeer(d, 2)
		p.Start()
		p.Stop()
		d.removePeer(p)
		if n := len(p.queue.outbound.c) + len(p.queue.controlOutbound.c); n != 0 {
			t.Errorf("%d stop sentinel(s) left in outbound queues", n)
		}
	}()
	for i := 0; i < 10; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFlushSkipsNilSentinel(t *testing.T) {
	d := testDevice(t)
	q := newAutodrainingOutboundQueue(d)
	q.c <- nil
	d.flushOutboundQueue(q)
	iq := newAutodrainingInboundQueue(d)
	iq.c <- nil
	d.flushInboundQueue(iq)
}

func container(d *Device, n int, control bool) *QueueOutboundElementsContainer {
	c := d.GetOutboundElementsContainer()
	c.isControl = control
	for i := 0; i < n; i++ {
		c.elems = append(c.elems, d.NewOutboundElement())
	}
	return c
}

func TestDataInFlightCap(t *testing.T) {
	d := idleDevice()
	p := testPeer(d, 2)
	for i := 0; i < maxDataInFlight/64; i++ {
		p.submit(container(d, 64, false), false)
	}
	if got := p.dataInFlight.Load(); got != maxDataInFlight {
		t.Fatalf("dataInFlight = %d, want %d", got, maxDataInFlight)
	}
	before := d.stats.txQueueFull.Load()
	p.submit(container(d, 1, false), false)
	if d.stats.txQueueFull.Load() != before+1 {
		t.Fatal("data beyond maxDataInFlight was not dropped")
	}
	p.submit(container(d, 1, true), true)
	if d.stats.txQueueFull.Load() != before+1 || len(p.queue.controlOutbound.c) != 1 {
		t.Fatal("control packet was held to the data cap")
	}
}

func TestCloseAllTunsStopsWriters(t *testing.T) {
	d := testDevice(t)
	w := newTunWriter(3)
	d.router.tuns[3] = &tunEntry{dev: nopTun{}, name: "t3", started: true, writer: w}
	d.router.closeAllTuns()
	select {
	case <-w.stop:
	default:
		t.Fatal("writer not stopped")
	}
}

type nopTun struct{}

func (nopTun) File() *os.File                         { return nil }
func (nopTun) Read([][]byte, []int, int) (int, error) { return 0, os.ErrClosed }
func (nopTun) Write(b [][]byte, _ int) (int, error)   { return len(b), nil }
func (nopTun) MTU() (int, error)                      { return 1416, nil }
func (nopTun) Name() (string, error)                  { return "t3", nil }
func (nopTun) Events() <-chan tun.Event               { return nil }
func (nopTun) Close() error                           { return nil }
func (nopTun) BatchSize() int                         { return 1 }
