package main

import (
	"flag"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	"melnode/dpproto"
)

func main() {
	socket := flag.String("socket", "", "path of the unix socket melnode-cp attaches to (required)")
	pprofAddr := flag.String("pprof", "", "if set, serve net/http/pprof on this address (e.g. 127.0.0.1:16061) -- also enables block/mutex profiling, which has real overhead, so leave this unset for normal (non-profiling) runs")
	flag.Parse()

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

	srv, err := dpproto.NewServer(*socket, func(s *dpproto.Session) dpproto.Handler {
		return &ctlHandler{sess: s}
	})
	if err != nil {
		log.Fatalf("control socket: failed to listen on %s: %v", *socket, err)
	}
	go srv.Run()
	log.Printf("control socket listening on %s (each connection is an independent data plane instance)", *socket)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received %v, shutting down", sig)
	srv.Close()
	log.Print("exiting")
}
