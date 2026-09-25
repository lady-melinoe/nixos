// melnode-cp is melnode's control plane: the slow-path half of the mesh VPN.
// It never touches a data packet. It owns
//
//   - link liveness (linkmonitor.go): a BFD-like session per link, carried as
//     proto=1 packets;
//   - path-vector routing (pathvector.go): which destinations are reachable
//     and via which link, carried as proto=2 packets;
//   - the host side of it all (reconcile.go): interface addresses, hooks,
//     kernel routes for advertised prefixes; and
//   - the local control and read-only introspection APIs.
//
// It puppets the data plane (melnode-dp) over a unix socket (see dpproto):
// it configures the device (identity, key, port, MTU -- the data plane has no
// config of its own), tells it which links exist and where to dial them,
// creates and destroys tuns, and installs or repoints next-hop routes; the data plane hands it
// every non-proto-0 packet it receives, and sends whatever this process
// injects. Think ovs-vswitchd talking to the kernel datapath.
//
// This process can also own starting the data plane (config:
// dataplaneCommand): it adopts one that's running, or starts one detached from
// itself. If this process restarts, the data plane keeps forwarding; on attach
// it is re-synced to the configuration. If the data plane restarts, this
// process re-attaches and re-programs it.
//
// Build:
//
//	go build ./cmd/melnode-cp
//
// Run (needs CAP_NET_ADMIN for interface/route configuration):
//
//	sudo ./melnode-cp -config cp.toml
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "", "path to the control plane's TOML config")
	verbose := flag.Bool("verbose", false, "log link and route events and other per-event detail (default: errors only)")
	stopDP := flag.Bool("stop-dataplane", false, "ask the running data plane to exit, then exit (stopping the control plane on its own deliberately leaves the data plane forwarding)")
	flag.Parse()

	if *configPath == "" {
		log.Fatal("-config is required")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	if *stopDP {
		if err := stopDataplane(cfg); err != nil {
			log.Fatalf("stop-dataplane: %v", err)
		}
		log.Print("data plane stopped")
		return
	}

	node, err := newNode(cfg, *verbose)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	// Every link's liveness transitions drive path-vector. Wired before any
	// monitor can start: otherwise an early Down->Init->Up (the other side
	// was already up and dialing us) could fire before anything is
	// listening for it, and that neighbor would never join the protocol
	// until its next flap.
	pathVector := node.pathVector
	for _, l := range node.links {
		l.monitor.SetOnStateChange(func(link *Link, next linkState) {
			switch next {
			case linkStateUp:
				pathVector.OnLinkUp(link)
			case linkStateDown, linkStateAdminDown:
				// Both mean "not usable right now" from path-vector's
				// point of view -- AdminDown (a deliberate stop) needs
				// the same immediate withdrawal a lost link gets, see
				// linkmonitor.go's AdminDown doc.
				pathVector.OnLinkDown(link)
			}
		})
	}

	pathVector.Start()

	// Local control-plane input (controlapi.go): an external process (a
	// container/VM scheduler) tells us what to advertise, over
	// HTTP-over-a-Unix-socket. Disabled by default.
	var controlAPIServer *controlAPI
	if cfg.ControlSocket != "" {
		controlAPIServer, err = newControlAPI(pathVector, cfg.ControlSocket)
		if err != nil {
			log.Fatalf("control API: failed to listen on %s: %v", cfg.ControlSocket, err)
		}
		controlAPIServer.Start()
		log.Printf("control API listening on %s", cfg.ControlSocket)
	}

	// Read-only introspection over TCP (introspectapi.go). Separate from
	// the control socket: no write endpoints are mounted on it.
	var introspectAPIServer *introspectAPI
	if cfg.IntrospectListen != "" {
		introspectAPIServer, err = newIntrospectAPI(pathVector, cfg.IntrospectListen, time.Duration(cfg.IntrospectIntervalMs)*time.Millisecond)
		if err != nil {
			log.Fatalf("introspect API: failed to listen on %s: %v", cfg.IntrospectListen, err)
		}
		introspectAPIServer.Start()
		log.Printf("introspect API (read-only) listening on tcp %s", cfg.IntrospectListen)
	}

	// Attach to the data plane (and re-attach whenever it goes away).
	stop := make(chan struct{})
	sessionsDone := make(chan struct{})
	go func() {
		defer close(sessionsDone)
		node.runSessions(cfg.DataplaneSocket, stop)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received %v, shutting down (the data plane keeps forwarding)", sig)

	close(stop)
	<-sessionsDone
	if controlAPIServer != nil {
		controlAPIServer.Stop()
	}
	if introspectAPIServer != nil {
		introspectAPIServer.Stop()
	}
	pathVector.Stop()
}
