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
// It boots knowing only the path of its control socket, and does nothing
// until melnode-cp configures it (dpproto's DeviceSet: node id, private key,
// UDP port, fwmark, MTU), the way `wg set` gives a WireGuard interface its
// key. From then on it does only what melnode-cp tells it to: which links
// exist, which tuns, which next hops.
//
// Any packet with a non-zero proto is control traffic. The data plane never
// interprets it: it punts it over the control socket to melnode-cp (the slow
// path: link liveness, path-vector routing, tun/route programming, host
// integration), and transmits whatever melnode-cp injects. The split is the
// one between an OVS-style kernel datapath and its userspace daemon, with
// both halves in userspace for now.
//
// This process keeps forwarding, with whatever links/tuns/routes it was last
// given, if the control plane goes away or restarts. melnode-cp can also
// start it (adopting one that is already running).
//
// Build:
//
//	go build ./cmd/melnode-dp
//
// Run (needs root/CAP_NET_ADMIN for the TUN devices):
//
//	sudo ./melnode-dp -socket /run/melnode/dp.sock
//
// Keys are plain WireGuard keys: generate one with `wg genkey`, and the
// matching public key with `wg pubkey`.
//
// Profile a running node:
//
//	./melnode-dp -socket ... -pprof 127.0.0.1:16061
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
	"sync"
	"syscall"

	"melnode/dpproto"
)

func main() {
	socket := flag.String("socket", "", "path of the unix socket melnode-cp attaches to (required)")
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

	if *socket == "" {
		log.Fatal("-socket is required")
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

	quit := make(chan struct{})
	var quitOnce sync.Once
	h := &ctlHandler{quit: func() { quitOnce.Do(func() { close(quit) }) }}
	srv, err := dpproto.NewServer(*socket, h)
	if err != nil {
		log.Fatalf("control socket: failed to listen on %s: %v", *socket, err)
	}
	h.srv = srv
	go srv.Run()
	log.Printf("control socket listening on %s (waiting for melnode-cp to configure this data plane)", *socket)

	// SIGINT/SIGTERM, or a Shutdown request from melnode-cp, triggers a
	// clean shutdown (stops every peer, closes every tun, closes the udp
	// socket and the control socket) instead of just dying mid-syscall.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		log.Printf("received %v, shutting down", sig)
	case <-quit:
		log.Print("shutdown requested by the control plane")
	}
	if dev := h.current(); dev != nil {
		dev.Close()
	} else {
		srv.Close()
	}
	log.Print("exiting")
}
