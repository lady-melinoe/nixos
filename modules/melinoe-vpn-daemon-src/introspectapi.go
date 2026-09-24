package main

import (
	"net"
	"net/http"
	"time"
)

// introspectapi.go serves melnode's read-only "show ..." endpoints
// (introspect.go) over TCP, so an operator on any node can look at any
// other node's view of the mesh (curl http://<node-host-addr>:60198/links).
//
// This is deliberately a *separate* http.Server with its own mux, not the
// control socket exposed on a TCP port: the mux built here only ever gets
// registerReadEndpoints, so /advertise and /withdraw do not exist on this
// listener at all (they 404), and every read handler is additionally
// GET-only. There is no authentication; reachability is restricted by the
// host firewall (nftables specialHostAccess: host range only).
type introspectAPI struct {
	pv       *PathVector
	listener net.Listener
	server   *http.Server
}

func newIntrospectAPI(pv *PathVector, addr string) (*introspectAPI, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	registerReadEndpoints(mux, pv, false)
	return &introspectAPI{
		pv:       pv,
		listener: l,
		server: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}, nil
}

func (a *introspectAPI) Start() {
	go func() {
		if err := a.server.Serve(a.listener); err != nil && err != http.ErrServerClosed {
			a.pv.dev.log.Errorf("introspect API: Serve failed: %v", err)
		}
	}()
}

func (a *introspectAPI) Stop() {
	_ = a.server.Close() // also closes a.listener
}
