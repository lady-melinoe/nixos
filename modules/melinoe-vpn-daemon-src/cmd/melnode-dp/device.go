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

// Device is the process-wide state of the data plane: our identity, the UDP
// socket, every configured peer, and the pools/queues the ported wireguard-go
// pipeline needs. This keeps wireguard-go's own field shape for staticIdentity/
// peers/indexTable/cookieChecker/rate/pool/queue (send.go, receive.go,
// noise-protocol.go, cookie.go all reach into these directly per
// PROJECT_STATE.md) but drops what melnode doesn't need: allowedips (we
// route by our own 4-byte header + Router.routeTable instead), UAPI,
// dynamic up/down/bind-rebinding, and mobile/route-listener quirks --
// melnode starts everything once from a static TOML config and runs
// until killed.
type Device struct {
	localID uint32 // 0-255, from config `localID`
	mtu     int    // MTU of every peer tun (config: mtu), also bounds padding -- see RoutineEncryption

	staticIdentity struct {
		sync.RWMutex
		privateKey NoisePrivateKey
		publicKey  NoisePublicKey
	}

	net struct {
		sync.RWMutex
		stopping sync.WaitGroup // referenced by ported receive.go's RoutineReceiveIncoming; melnode never tears down the bind mid-run, so this just needs to exist, not do anything
		bind     conn.Bind
	}

	peers struct {
		sync.RWMutex
		keyMap map[NoisePublicKey]*Peer
	}
	peersByID sync.Map // uint32 (link peerid) -> *Peer, for router.go's lookupPeerByID

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
		controlEncryption *outboundQueue // separate from encryption -- see Peer.submit (send.go) / PROJECT_STATE.md's backpressure section
		decryption        *inboundQueue
		handshake         *handshakeQueue

		// dataSubmitMu/controlSubmitMu guard Peer.submit's atomic
		// check-both-then-send into a peer's outbound queue and this
		// device's (shared, per-class) encryption queue -- see
		// submit's own doc comment and PROJECT_STATE.md for why these
		// exist: an earlier version left the encryption-queue send
		// unguarded and blocking, which measurably backed up under
		// real multi-Gbit/s load into multiple GB of legitimately
		// live (not leaked) in-flight data, stalling packet intake
		// upstream. Device-wide (not per-peer) because encryption.c/
		// controlEncryption.c are themselves shared across every peer.
		dataSubmitMu    sync.Mutex
		controlSubmitMu sync.Mutex

		// decryptionSubmitMu guards the mirror-image atomic
		// check-both-then-send on the *receive* side (RoutineReceiveIncoming,
		// receive.go): peer.queue.inbound.c (per-peer) and
		// device.queue.decryption.c (shared). Found the same way as
		// dataSubmitMu above, just one step later -- the send-side fix
		// alone didn't resolve the originally reported symptom because
		// the specific repro had the *receiving* node as the
		// bottleneck, not the sender; this is the same blocking-send
		// anti-pattern on the socket-read side instead of the
		// tun-read side. See PROJECT_STATE.md.
		decryptionSubmitMu sync.Mutex
	}

	router *Router // set once, after both Device and Router are constructed in main.go

	// stats are the datapath's drop/traffic counters, dumped by Stats (ctl.go).
	// Only ever bumped on drop paths, so they cost nothing on the fast path.
	stats struct {
		rxNoRoute, rxTTL, rxNoTun, rxTunFull, rxBad, rxQueueFull atomic.Uint64
		txNoRoute, txQueueFull                                   atomic.Uint64
		injectSent, injectDropped                                atomic.Uint64
	}

	ctl *dpproto.Server // the control plane's side of the socket (ctl.go); set once in main.go

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

// batchSize mirrors wireguard-go's Device.BatchSize(): the max of the
// bind's batch size and the largest of our tuns' batch sizes (melnode has
// N tuns, not one -- see router.go). Must be called after the bind and
// all peer tuns exist.
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
	if device.ctl != nil {
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

// removePeer forgets a link. The caller must have Stop()ped it first.
func (device *Device) removePeer(p *Peer) {
	device.peers.Lock()
	if device.peers.keyMap[p.handshake.remoteStatic] == p {
		delete(device.peers.keyMap, p.handshake.remoteStatic)
	}
	device.peers.Unlock()
	device.peersByID.CompareAndDelete(p.id, p)
}

// IsUnderLoad reports whether the handshake queue is currently backed up
// enough that we should start demanding cookie replies (see receive.go's
// use of this, gating CheckMAC2 + the per-source rate limiter). This
// used to be hardcoded false -- meaning neither cookie/mac2 checking nor
// the per-source-address rate limiter (device.rate.limiter.Allow) ever
// engaged at all, since both are nested inside `if
// device.IsUnderLoad()` in receive.go. The only gate on a handshake
// initiation was CheckMAC1, which just proves the sender knows our
// static public key -- cheap to pass, and in a mesh where peer pubkeys
// aren't exactly secret, that left every node wide open to an
// unthrottled flood of full X25519+ChaCha20Poly1305 decrypt attempts.
// Ported verbatim from wireguard-go's own device.go IsUnderLoad -- once
// the handshake queue crosses 1/8 full, we're "under load" for at least
// UnderLoadAfterTime after the last time that was true, same as
// upstream.
func (device *Device) IsUnderLoad() bool {
	now := time.Now()
	underLoad := len(device.queue.handshake.c) >= QueueHandshakeSize/8
	if underLoad {
		device.rate.underLoadUntil.Store(now.Add(UnderLoadAfterTime).UnixNano())
		return true
	}
	return device.rate.underLoadUntil.Load() > now.UnixNano()
}

// startCryptoWorkers starts the shared, device-wide encrypt/decrypt/
// handshake worker pools -- exactly as wireguard-go's NewDevice does,
// just pulled out into its own step since melnode's device/peer
// construction is split across newDevice/newPeer/addPeer rather than one
// UAPI-driven NewDevice call.
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
