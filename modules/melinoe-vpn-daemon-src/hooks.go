package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// hookTimeout bounds how long a tun create/destroy hook may run. Hooks run
// synchronously from the router (path-vector goroutines / shutdown), so a
// hung hook must not be able to wedge route handling forever.
const hookTimeout = 10 * time.Second

// runTunHook runs the configured hook binary (if any) as
// `<bin> <peerID> <ifname>`. Failures are logged, never fatal: a broken
// hook must not take the tunnel down with it. kind is "create"/"destroy",
// used only for logging.
func (r *Router) runTunHook(kind, bin string, peerID uint32, ifname string) {
	if bin == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, strconv.FormatUint(uint64(peerID), 10), ifname)
	out, err := cmd.CombinedOutput()
	if o := strings.TrimSpace(string(out)); o != "" {
		r.dev.log.Verbosef("router: tun %s hook %q (peerid %d, %s) output: %s", kind, bin, peerID, ifname, o)
	}
	if err != nil {
		r.dev.log.Errorf("router: tun %s hook %q (peerid %d, %s) failed: %v", kind, bin, peerID, ifname, err)
		return
	}
	r.dev.log.Verbosef("router: tun %s hook %q (peerid %d, %s) ok", kind, bin, peerID, ifname)
}
