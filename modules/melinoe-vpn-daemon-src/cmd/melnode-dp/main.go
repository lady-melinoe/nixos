// melnode-dp is melnode's data plane: the WireGuard-like "kernel" half of the
// mesh VPN. It is deliberately not a router in the interesting sense -- it
// never decides anything on its own. It owns
//
//   - the UDP socket and the Noise IK handshakes/keypairs for every link,
//   - the encrypt/decrypt pipeline,
//   - the per-destination tun devices, and
//   - the dst-peerid -> next-hop-link table,
//
// and forwards tunneled IP packets (proto 0 in melnode's 4-byte routing
// header) entirely by itself: tun -> encrypt -> next-hop link, and link ->
// decrypt -> local tun or re-encrypt onto the next hop.
//
// Any packet with a non-zero proto is control traffic. The data plane never
// interprets it: it punts it over the control socket to melnode-cp (the slow
// path: link liveness, path-vector routing, tun/route programming, host
// integration), and transmits whatever melnode-cp injects. When melnode-cp
// wants a link added, a tun created or a next hop changed, it tells this
// process to do it (see dpproto). The split is the one between an OVS-style
// kernel datapath and its userspace daemon, with both halves in userspace
// for now.
//
// This process keeps forwarding, with whatever links/tuns/routes it was last
// given, if the control plane goes away or restarts.
//
// Build:
//
//	go build ./cmd/melnode-dp
//
// Run (needs root/CAP_NET_ADMIN for the TUN devices):
//
//	sudo ./melnode-dp -config dp.toml
//
// Generate a keypair for a new node:
//
//	./melnode-dp -genkey
//
// Profile a running node:
//
//	./melnode-dp -config dp.toml -pprof 127.0.0.1:16061
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

	"melnode/dpproto"
)

func main() {
	configPath := flag.String("config", "", "path to the data plane's TOML config")
	genkey := flag.Bool("genkey", false, "generate a new private/public keypair (base64) and exit -- for populating localPrivkey / peerPubkey")
	verbose := flag.Bool("verbose", false, "log handshakes, peer start/stop, and other per-event detail (default: errors only)")
	pprofAddr := flag.String("pprof", "", "if set, serve net/http/pprof on this address (e.g. 127.0.0.1:16061) -- also enables block/mutex profiling, which has real overhead, so leave this unset for normal (non-profiling) runs")
	flag.Parse()

	// Raise GOGC from Go's default (100) unless the environment already
	// overrides it -- a generically reasonable default for a
	// throughput-sensitive network daemon (fewer, cheaper collections).
	// No memory limit is set alongside it: a legitimately backed-up
	// pipeline (Peer.queue.outbound + device.queue.encryption at
	// capacity) can genuinely hold several GB of live buffers, and a
	// tight limit would just fight the GC against perfectly legitimate
	// bounded data (see send.go's Peer.submit for the bound).
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
	dev.mtu = cfg.MTU
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

	// The forwarding state starts empty: no links, no tuns, no routes. All of
	// it arrives over the control socket. (Set up before the receive
	// goroutines start, since they punt through dev.ctl.)
	dev.router = newRouter(dev, uint32(cfg.LocalID), cfg.TunPrefix)

	ctl, err := dpproto.NewServer(cfg.Socket, &ctlHandler{dev: dev, port: uint16(cfg.LocalPort)})
	if err != nil {
		log.Fatalf("control socket: failed to listen on %s: %v", cfg.Socket, err)
	}
	dev.ctl = ctl
	go ctl.Run()
	log.Printf("control socket listening on %s (waiting for melnode-cp)", cfg.Socket)

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
	// every peer, closes every tun, closes the udp socket and the control
	// socket) instead of just dying mid-syscall.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down", sig)
		dev.Close()
	}()

	<-dev.closed
	log.Print("device closed, exiting")
}
