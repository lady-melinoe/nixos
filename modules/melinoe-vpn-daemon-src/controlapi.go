package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
)

// controlapi.go is melnode's local control-plane input (config:
// controlSocket): a tiny HTTP-over-Unix-socket API an external process
// (a container/VM scheduler, per PROJECT_STATE.md) uses to tell melnode
// which prefixes it's now responsible for. Deliberately not a
// network-reachable port: this is a *local* control surface, not
// something other mesh nodes ever talk to -- they only ever see the
// resulting proto=2 announcements (pathvector.go), same as any other
// route. HTTP-over-Unix-socket rather than a raw line protocol since
// net/http is already in the binary (for -pprof) and JSON leaves room
// for richer requests later without another wire-format bump.
type controlAPI struct {
	pv       *PathVector
	listener net.Listener
	server   *http.Server
}

type prefixRequest struct {
	Prefix string `json:"prefix"`
}

func newControlAPI(pv *PathVector, socketPath string) (*controlAPI, error) {
	_ = os.Remove(socketPath) // stale socket from a previous crashed run
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}

	c := &controlAPI{pv: pv, listener: l}
	mux := http.NewServeMux()
	mux.HandleFunc("/advertise", c.handleAdvertise)
	mux.HandleFunc("/withdraw", c.handleWithdraw)
	c.server = &http.Server{Handler: mux}
	return c, nil
}

func (c *controlAPI) Start() {
	go func() {
		if err := c.server.Serve(c.listener); err != nil && err != http.ErrServerClosed {
			c.pv.dev.log.Errorf("control API: Serve failed: %v", err)
		}
	}()
}

func (c *controlAPI) Stop() {
	_ = c.server.Close() // also closes c.listener
}

func (c *controlAPI) handleAdvertise(w http.ResponseWriter, r *http.Request) {
	c.handlePrefixRequest(w, r, c.pv.AdvertisePrefix)
}

func (c *controlAPI) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	c.handlePrefixRequest(w, r, c.pv.WithdrawPrefix)
}

func (c *controlAPI) handlePrefixRequest(w http.ResponseWriter, r *http.Request, apply func(pvPrefix)) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req prefixRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	prefix, ok := parsePrefix(req.Prefix)
	if !ok {
		http.Error(w, "invalid prefix: "+req.Prefix, http.StatusBadRequest)
		return
	}
	apply(prefix)
	w.WriteHeader(http.StatusOK)
}
