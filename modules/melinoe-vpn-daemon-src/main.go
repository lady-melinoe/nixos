// melnode is a mesh router daemon: it reads a config-N.toml describing
// this node's links, brings up one Noise IK tunnel per [[link]], and
// dynamically discovers which other nodes are reachable (and via which
// link) using a BFD-like liveness check per link (linkmonitor.go) and a
// path-vector routing protocol on top of that (pathvector.go) -- see
// PROJECT_STATE.md. TUN interfaces are created/destroyed on demand, one
// per discovered destination, not from static config.
//
// Build:
//
//	go mod tidy   # first time only -- fetches dependencies
//	go build -o meltun .
//
// Run (needs root/CAP_NET_ADMIN for the TUN devices):
//
//	sudo ./meltun -config example-node1.toml
//
// Generate a keypair for a new node's config:
//
//	./meltun -genkey
//
// Profile a running node (see setup_netns_melnode.py --profile-test for
// an automated harness that does this against a live iperf3 run):
//
//	./meltun -config example-node1.toml -pprof 127.0.0.1:16061
//	go tool pprof 'http://127.0.0.1:16061/debug/pprof/profile?seconds=10'
package main

import (
	"flag"
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* handlers on http.DefaultServeMux
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/tun"
)

func main() {
	configPath := flag.String("config", "", "path to config-N.toml")
	genkey := flag.Bool("genkey", false, "generate a new private/public keypair (base64) and exit -- for populating localPrivkey / peerPubkey")
	verbose := flag.Bool("verbose", false, "log handshakes, peer start/stop, and other per-event detail (default: errors only)")
	pprofAddr := flag.String("pprof", "", "if set, serve net/http/pprof on this address (e.g. 127.0.0.1:16061) -- also enables block/mutex profiling, which has real overhead, so leave this unset for normal (non-profiling) runs")
	flag.Parse()

	// Raise GOGC from Go's default (100) unless the environment already
	// overrides it -- a generically reasonable default for a
	// throughput-sensitive network daemon (fewer, cheaper collections).
	// This alone was an earlier, now-superseded hypothesis for a
	// reported symptom (heavy packet loss for the first few seconds of
	// a sudden jump to multi-gigabit throughput, then permanently
	// fine): the real cause turned out to be Peer.submit (send.go)
	// blocking on a shared, device-wide encryption queue that could
	// legitimately back up into multiple GB of in-flight data under
	// genuine encryption-throughput pressure -- not a GC-pacer
	// convergence issue, though the GC trace requested to check that
	// theory (see PROJECT_STATE.md) is exactly what surfaced the real
	// one: heap size doubling every collection while the backlog built
	// up. That's fixed at the source now (submit is atomic and
	// tail-drops instead of blocking), so no memory limit is set here
	// alongside GOGC -- an earlier version set one (512MiB) sized to
	// guard against *unbounded* growth from raising GOGC, which is the
	// wrong number now that a legitimate, fully-utilized backlog across
	// Peer.queue.outbound/device.queue.encryption's combined capacity
	// can genuinely be several GB on its own; a tight limit would just
	// fight the GC against perfectly legitimate bounded data instead of
	// guarding against anything real.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400)
	}

	if *genkey {
		genkeyAndExit()
		return
	}
	if *configPath == "" {
		log.Fatal("-config is required (or pass -genkey to generate a keypair)")
	}

	if *pprofAddr != "" {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)
		go func() {
			log.Printf("pprof: serving http://%s/debug/pprof/", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				log.Printf("pprof: server exited: %v", err)
			}
		}()
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	staticPrivate, err := cfg.privateKey()
	if err != nil {
		log.Fatalf("invalid private key: %v", err)
	}

	dev := newDevice(uint32(cfg.LocalID), staticPrivate)
	if *verbose {
		dev.log = NewLogger(LogLevelVerbose, "")
	}
	log.Printf("local id=%d, static pubkey=%s", cfg.LocalID, keyToBase64(dev.staticIdentity.publicKey[:]))
	dev.startCryptoWorkers()

	bind := conn.NewStdNetBind()
	receiveFuncs, actualPort, err := bind.Open(uint16(cfg.LocalPort))
	if err != nil {
		log.Fatalf("failed to bind udp socket on port %d: %v", cfg.LocalPort, err)
	}
	dev.net.bind = bind
	log.Printf("udp socket listening on port %d (batch size %d)", actualPort, bind.BatchSize())

	if cfg.Fwmark != 0 {
		if err := bind.SetMark(uint32(cfg.Fwmark)); err != nil {
			log.Fatalf("failed to set fwmark %d on udp socket: %v", cfg.Fwmark, err)
		}
		log.Printf("fwmark %d set on udp socket", cfg.Fwmark)
	}

	// --- bring up one Noise IK peer per [[link]] ---
	for _, l := range cfg.Links {
		pubkey, err := parsePubKeyBase64(l.PeerPubkey)
		if err != nil {
			log.Fatalf("[[link]] peerid %d: invalid peerPubkey: %v", l.PeerID, err)
		}

		var endpoint conn.Endpoint
		if l.Endpoint != "" {
			endpoint, err = bind.ParseEndpoint(l.Endpoint)
			if err != nil {
				log.Fatalf("[[link]] peerid %d: invalid endpoint %q (must be an ip:port literal -- hostnames aren't supported by conn.Bind): %v", l.PeerID, l.Endpoint, err)
			}
		}

		p, err := newPeer(dev, uint32(l.PeerID), pubkey, endpoint, uint32(l.PrependCount))
		if err != nil {
			log.Fatalf("[[link]] peerid %d: failed to set up peer: %v", l.PeerID, err)
		}
		dev.addPeer(p)

		role := "listen-only (no endpoint configured)"
		if endpoint != nil {
			role = "dialing " + endpoint.DstToString()
		}
		log.Printf("link -> peerid %d: %s", l.PeerID, role)
	}

	// --- routing is now entirely dynamic (path-vector, pathvector.go) --
	// Router starts with an empty route table and no tuns; both are
	// populated as routes are discovered, not from static [[peer]]
	// config (see PROJECT_STATE.md).
	var identityPrefix *pvPrefix
	if cfg.IdentityPrefix != "" {
		parsed, _ := parsePrefix(cfg.IdentityPrefix) // already validated in Config.validate
		identityPrefix = &parsed
	}
	router := newRouter(dev, uint32(cfg.LocalID), cfg.TunPrefix, identityPrefix, cfg.TunCreateHookBin, cfg.TunDestroyHookBin)
	dev.router = router

	pathVector := newPathVector(dev, uint32(cfg.LocalID))
	dev.pathVector = pathVector

	if identityPrefix != nil {
		pathVector.AdvertisePrefix(*identityPrefix)
		log.Printf("identity: always advertising %v (assigned to every tun this node owns)", *identityPrefix)
	}

	// Wire each link's liveness transitions to path-vector *before*
	// starting any peer -- otherwise an early Down->Init->Up (e.g. the
	// other side was already up and dialing us) could fire before
	// anything is listening for it, and that neighbor would never get
	// added to the protocol until its next flap.
	dev.peers.RLock()
	for _, p := range dev.peers.keyMap {
		p.monitor.SetOnStateChange(func(peer *Peer, next linkState) {
			switch next {
			case linkStateUp:
				pathVector.OnLinkUp(peer)
			case linkStateDown, linkStateAdminDown:
				// Both mean "not usable right now" from path-vector's
				// point of view -- AdminDown (e.g. peer.Stop()) needs
				// the same immediate withdrawal a lost link gets, see
				// linkmonitor.go's AdminDown doc.
				pathVector.OnLinkDown(peer)
			}
		})
	}
	dev.peers.RUnlock()

	pathVector.Start()

	// Local control-plane input (controlapi.go): an external process
	// (a container/VM scheduler, per PROJECT_STATE.md) tells melnode
	// what to advertise, over HTTP-over-a-Unix-socket. Disabled by
	// default -- no controlSocket configured means no local control
	// surface at all, only IdentityPrefix (if set) and mesh-discovered
	// peerid routes.
	var controlAPIServer *controlAPI
	if cfg.ControlSocket != "" {
		var err error
		controlAPIServer, err = newControlAPI(pathVector, cfg.ControlSocket)
		if err != nil {
			log.Fatalf("control API: failed to listen on %s: %v", cfg.ControlSocket, err)
		}
		controlAPIServer.Start()
		log.Printf("control API listening on %s", cfg.ControlSocket)
	}

	// Start each link peer: brings up its timer loop, its two per-peer
	// sequential sender/receiver goroutines (ported from wireguard-go's
	// device/peer.go Start()), and its BFD-like liveness session
	// (linkmonitor.go) -- which is what will actually bring the
	// path-vector neighbor relationship up once handshakes complete.
	dev.peers.RLock()
	for _, p := range dev.peers.keyMap {
		p.Start()
	}
	dev.peers.RUnlock()

	// One reader goroutine per ReceiveFunc bind.Open() gave us -- on
	// Linux/most platforms that's two (IPv4 and IPv6), each its own
	// socket under the hood (ported from wireguard-go's device/receive.go
	// RoutineReceiveIncoming). Every link still shares this one bind
	// (one UDP port for every peer); datagrams are demuxed by message
	// type and (for handshake responses / transport data) the embedded
	// receiver index, not by source address, since a peer can roam.
	dev.net.stopping.Add(len(receiveFuncs))
	dev.queue.decryption.wg.Add(len(receiveFuncs))
	dev.queue.handshake.wg.Add(len(receiveFuncs))
	for _, fn := range receiveFuncs {
		go dev.RoutineReceiveIncoming(bind.BatchSize(), fn)
	}

	// SIGINT/SIGTERM triggers a clean shutdown (Device.Close(): stops
	// every peer, closes every tun, closes the udp socket) instead of
	// just dying mid-syscall -- there was previously no signal handling
	// at all, so this used to only ever exit via an unhandled kill.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down", sig)
		if controlAPIServer != nil {
			controlAPIServer.Stop()
		}
		dev.Close()
	}()

	<-dev.closed
	log.Print("device closed, exiting")
}

// drainTunEvents drains a TUN device's Events() channel so its internal
// listener never blocks; we don't currently act on up/down/MTU events
// (no equivalent of wireguard-go's device/tun.go RoutineTUNEventReader --
// melnode has N tuns, one per currently-routable peerid (see
// router.go), not one tun whose up/down state drives the whole
// device's).
func drainTunEvents(dev tun.Device) {
	for range dev.Events() {
	}
}
