package main

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/ratelimiter"

	"melnode/dpproto"
)

var errNoKnownEndpoint = errors.New("no known remote address yet (listen-only peer, haven't heard from it)")

type Device struct {
	localID uint32
	mtu     int

	staticIdentity struct {
		sync.RWMutex
		privateKey NoisePrivateKey
		publicKey  NoisePublicKey
	}

	net struct {
		sync.RWMutex
		stopping sync.WaitGroup
		bind     conn.Bind
	}

	peers struct {
		sync.RWMutex
		keyMap map[NoisePublicKey]*Peer
	}
	peersByID sync.Map

	rate struct {
		underLoadUntil atomic.Int64
		limiter        ratelimiter.Ratelimiter
	}

	indexTable    IndexTable
	cookieChecker CookieChecker

	pool struct {
		inboundElementsContainer  *WaitPool
		outboundElementsContainer *WaitPool
		messageBuffers            *WaitPool
		inboundElements           *WaitPool
		outboundElements          *WaitPool
	}

	queue struct {
		encryption        *outboundQueue
		controlEncryption *outboundQueue
		decryption        *inboundQueue
		handshake         *handshakeQueue

		dataSubmitMu    sync.Mutex
		controlSubmitMu sync.Mutex

		decryptionSubmitMu sync.Mutex
	}

	router *Router

	stats struct {
		rxNoRoute, rxTTL, rxNoTun, rxTunFull, rxBad, rxQueueFull atomic.Uint64
		txNoRoute, txQueueFull                                   atomic.Uint64
		injectSent, injectDropped                                atomic.Uint64
	}

	keepCtl bool
	ctl     *dpproto.Server

	closedFlag atomic.Bool
	closed     chan struct{}

	log *Logger
}

func newDevice(localID uint32, staticPrivate NoisePrivateKey) *Device {
	d := &Device{
		localID: localID,
		mtu:     defaultMTU,
		closed:  make(chan struct{}),
		log:     NewLogger(LogLevelError, ""),
	}
	d.staticIdentity.privateKey = staticPrivate
	d.staticIdentity.publicKey = staticPrivate.publicKey()
	d.peers.keyMap = make(map[NoisePublicKey]*Peer)
	d.rate.limiter.Init()
	d.indexTable.Init()
	d.cookieChecker.Init(d.staticIdentity.publicKey)

	d.PopulatePools()

	d.queue.handshake = newHandshakeQueue()
	d.queue.encryption = newOutboundQueue()
	d.queue.controlEncryption = newOutboundQueue()
	d.queue.decryption = newInboundQueue()

	return d
}

func (device *Device) batchSize() int {
	size := 1
	if device.net.bind != nil {
		size = device.net.bind.BatchSize()
	}
	if device.router != nil {
		if s := device.router.maxTunBatchSize(); s > size {
			size = s
		}
	}
	return size
}

func (device *Device) BatchSize() int { return device.batchSize() }

func (device *Device) isClosed() bool { return device.closedFlag.Load() }
func (device *Device) isUp() bool     { return !device.isClosed() }

func (device *Device) Close() {
	if device.closedFlag.Swap(true) {
		return
	}
	device.peers.RLock()
	for _, peer := range device.peers.keyMap {
		peer.Stop()
	}
	device.peers.RUnlock()
	if device.ctl != nil && !device.keepCtl {
		device.ctl.Close()
	}
	if device.router != nil {
		device.router.closeAllTuns()
	}
	if device.net.bind != nil {
		if err := device.net.bind.Close(); err != nil {
			device.log.Errorf("closing udp socket: %v", err)
		}
	}
	device.queue.encryption.wg.Done()
	device.queue.controlEncryption.wg.Done()
	device.queue.decryption.wg.Done()
	device.queue.handshake.wg.Done()
	device.rate.limiter.Close()
	close(device.closed)
}

func (device *Device) LookupPeer(pk NoisePublicKey) *Peer {
	device.peers.RLock()
	defer device.peers.RUnlock()
	return device.peers.keyMap[pk]
}

func (device *Device) lookupPeerByID(id uint32) *Peer {
	v, ok := device.peersByID.Load(id)
	if !ok {
		return nil
	}
	return v.(*Peer)
}

func (device *Device) addPeer(p *Peer) {
	device.peers.Lock()
	device.peers.keyMap[p.handshake.remoteStatic] = p
	device.peers.Unlock()
	device.peersByID.Store(p.id, p)
}

func (device *Device) removePeer(p *Peer) {
	device.peers.Lock()
	if device.peers.keyMap[p.handshake.remoteStatic] == p {
		delete(device.peers.keyMap, p.handshake.remoteStatic)
	}
	device.peers.Unlock()
	device.peersByID.CompareAndDelete(p.id, p)
}

func (device *Device) IsUnderLoad() bool {
	now := time.Now()
	underLoad := len(device.queue.handshake.c) >= QueueHandshakeSize/8
	if underLoad {
		device.rate.underLoadUntil.Store(now.Add(UnderLoadAfterTime).UnixNano())
		return true
	}
	return device.rate.underLoadUntil.Load() > now.UnixNano()
}

func (device *Device) startCryptoWorkers() {
	cpus := runtime.NumCPU()
	device.queue.encryption.wg.Add(cpus)
	device.queue.controlEncryption.wg.Add(cpus)
	for i := 0; i < cpus; i++ {
		go device.RoutineEncryption(i + 1)
		go device.RoutineDecryption(i + 1)
		go device.RoutineHandshake(i + 1)
	}
}
