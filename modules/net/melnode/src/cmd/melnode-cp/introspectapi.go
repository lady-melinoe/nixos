package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type introspectAPI struct {
	pv       *PathVector
	listener net.Listener
	server   *http.Server
	interval time.Duration

	snap atomic.Pointer[introspectSnapshot]
	stop chan struct{}
	wg   sync.WaitGroup
}

type introspectSnapshot struct {
	generatedAt time.Time
	bodies      map[string][]byte
}

func newIntrospectAPI(pv *PathVector, addr string, interval time.Duration) (*introspectAPI, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	a := &introspectAPI{pv: pv, listener: l, interval: interval, stop: make(chan struct{})}
	mux := http.NewServeMux()
	for _, path := range readPaths {
		mux.HandleFunc(path, getOnly(a.serve(path)))
	}
	mux.HandleFunc("/", getOnly(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"read": readPaths})
	}))
	a.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return a, nil
}

func (a *introspectAPI) Start() {
	a.refresh()
	a.wg.Add(1)
	go a.refreshLoop()
	go func() {
		if err := a.server.Serve(a.listener); err != nil && err != http.ErrServerClosed {
			a.pv.node.log.Errorf("introspect API: Serve failed: %v", err)
		}
	}()
}

func (a *introspectAPI) Stop() {
	_ = a.server.Close()
	close(a.stop)
	a.wg.Wait()
}

func (a *introspectAPI) refreshLoop() {
	defer a.wg.Done()
	tick := time.NewTicker(a.interval)
	defer tick.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-tick.C:
			a.refresh()
		}
	}
}

func (a *introspectAPI) refresh() {
	v := a.pv.node.readDP(true)
	if v.err != nil && a.snap.Load() != nil {
		a.pv.node.log.Verbosef("introspect API: keeping the previous snapshot: %v", v.err)
		return
	}
	views := a.pv.buildViews(v)
	s := &introspectSnapshot{generatedAt: views.generatedAt, bodies: make(map[string][]byte, len(views.byPath))}
	for path, view := range views.byPath {
		b, err := json.MarshalIndent(view, "", "  ")
		if err != nil {
			a.pv.node.log.Errorf("introspect API: encoding %s: %v", path, err)
			return
		}
		s.bodies[path] = append(b, '\n')
	}
	a.snap.Store(s)
}

func (a *introspectAPI) serve(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		s := a.snap.Load()
		if s == nil {
			http.Error(w, "no snapshot yet", http.StatusServiceUnavailable)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Age", strconv.Itoa(int(time.Since(s.generatedAt).Seconds())))
		h.Set("X-Melnode-Generated-At", s.generatedAt.UTC().Format(time.RFC3339Nano))
		_, _ = w.Write(s.bodies[path])
	}
}
