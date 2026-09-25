package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const hookTimeout = 10 * time.Second

func (r *Router) runTunHook(kind, bin string, peerID uint32, ifname string) {
	if bin == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, strconv.FormatUint(uint64(peerID), 10), ifname)
	out, err := cmd.CombinedOutput()
	if o := strings.TrimSpace(string(out)); o != "" {
		r.node.log.Verbosef("router: tun %s hook %q (peerid %d, %s) output: %s", kind, bin, peerID, ifname, o)
	}
	if err != nil {
		r.node.log.Errorf("router: tun %s hook %q (peerid %d, %s) failed: %v", kind, bin, peerID, ifname, err)
		return
	}
	r.node.log.Verbosef("router: tun %s hook %q (peerid %d, %s) ok", kind, bin, peerID, ifname)
}
