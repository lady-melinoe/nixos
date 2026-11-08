package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const hookTimeout = 10 * time.Second

// runPeerHook runs the create or destroy hook of a node: it gets the node's id, the device's name
// and the address routes to the node go via.
func (r *Router) runPeerHook(kind, bin string, peerID uint32, ifname, gateway string) error {
	if bin == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, strconv.FormatUint(uint64(peerID), 10), ifname, gateway)
	out, err := cmd.CombinedOutput()
	if o := strings.TrimSpace(string(out)); o != "" {
		r.node.log.Verbosef("router: peer %s hook %q (peerid %d, %s via %s) output: %s", kind, bin, peerID, ifname, gateway, o)
	}
	if err != nil {
		return fmt.Errorf("peer %s hook %q: %w", kind, bin, err)
	}
	r.node.log.Verbosef("router: peer %s hook %q (peerid %d, %s via %s) ok", kind, bin, peerID, ifname, gateway)
	return nil
}
