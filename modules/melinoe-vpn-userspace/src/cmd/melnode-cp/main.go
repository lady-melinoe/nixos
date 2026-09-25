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

	pathVector := node.pathVector
	for _, l := range node.links {
		l.monitor.SetOnStateChange(func(link *Link, next linkState) {
			switch next {
			case linkStateUp:
				pathVector.OnLinkUp(link)
			case linkStateDown, linkStateAdminDown:
				pathVector.OnLinkDown(link)
			}
		})
	}

	pathVector.Start()

	var controlAPIServer *controlAPI
	if cfg.ControlSocket != "" {
		controlAPIServer, err = newControlAPI(pathVector, cfg.ControlSocket)
		if err != nil {
			log.Fatalf("control API: failed to listen on %s: %v", cfg.ControlSocket, err)
		}
		controlAPIServer.Start()
		log.Printf("control API listening on %s", cfg.ControlSocket)
	}

	var introspectAPIServer *introspectAPI
	if cfg.IntrospectListen != "" {
		introspectAPIServer, err = newIntrospectAPI(pathVector, cfg.IntrospectListen, time.Duration(cfg.IntrospectIntervalMs)*time.Millisecond)
		if err != nil {
			log.Fatalf("introspect API: failed to listen on %s: %v", cfg.IntrospectListen, err)
		}
		introspectAPIServer.Start()
		log.Printf("introspect API (read-only) listening on tcp %s", cfg.IntrospectListen)
	}

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
