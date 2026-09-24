package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"melnode/dpproto"
)

// spawn.go: getting a data plane running when this process owns that
// (config: dataplaneCommand). The model is ovs-ctl starting ovs-vswitchd, or
// `ip link add type wireguard` bringing up the kernel side: the control plane
// makes sure the datapath exists, but the datapath outlives it.
//
//   - A data plane that is already there is adopted, not replaced -- unless
//     it is running a different binary than configured, or was configured
//     differently (see replaceDataplane).
//   - Otherwise one is started, in its own session (so it is not tied to our
//     terminal or process group) and never killed by us: when this process
//     exits or restarts, the data plane keeps forwarding.

// spawnWait is how long after starting the data plane we keep trying to
// reach its socket before giving up on this attempt.
const spawnWait = 5 * time.Second

// dialDataplane connects to the data plane, starting it first if we own that
// and it isn't running.
func (n *Node) dialDataplane(socket string, onPunt func(dpproto.Punt), onEvent func(dpproto.Event)) (*dpproto.Client, error) {
	cl, err := dpproto.Dial(socket, onPunt, onEvent)
	if err == nil || len(n.dpCommand) == 0 {
		return cl, err
	}
	// Only "nothing is listening" means start one; anything else (say,
	// permission denied on the socket) starting another wouldn't fix.
	if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, err
	}
	if err := n.spawnDataplane(socket); err != nil {
		return nil, fmt.Errorf("starting data plane: %w", err)
	}
	deadline := time.Now().Add(spawnWait)
	for {
		if cl, err = dpproto.Dial(socket, onPunt, onEvent); err == nil {
			return cl, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("started the data plane but it never came up on %s: %w", socket, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// spawnDataplane starts the data plane, detached, unless the one we started
// earlier is still running (i.e. still coming up).
func (n *Node) spawnDataplane(socket string) error {
	n.childMu.Lock()
	defer n.childMu.Unlock()
	if n.child != nil {
		select {
		case <-n.child:
		default:
			return nil // still running; just wait for its socket
		}
	}

	argv := n.dpCommand
	args := append(append([]string(nil), argv[1:]...), "-socket", socket)
	if n.verbose {
		args = append(args, "-verbose")
	}
	cmd := exec.Command(argv[0], args...)
	// Inherit our stdout/stderr, so the data plane's log ends up wherever ours does.
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	// Its own session: our terminal's signals aren't its problem, and it is
	// not in our process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	n.log.Verbosef("started data plane %v (pid %d)", argv, cmd.Process.Pid)

	done := make(chan struct{})
	n.child = done
	go func() {
		// Reap it so an exit doesn't leave a zombie while we're running.
		err := cmd.Wait()
		n.log.Errorf("data plane (pid %d) exited: %v", cmd.Process.Pid, err)
		close(done)
	}()
	return nil
}

// wrongBinary reports whether the process behind pid is running a different
// executable than the one we're configured to start. Unknowable (no /proc, no
// permission) counts as fine: we only replace what we can positively tell is
// stale. A binary replaced on disk shows up as "... (deleted)", which differs
// too.
func (n *Node) wrongBinary(pid uint32) bool {
	if len(n.dpCommand) == 0 || pid == 0 {
		return false
	}
	have, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false
	}
	want, err := exec.LookPath(n.dpCommand[0])
	if err != nil {
		return false
	}
	if w, err := filepath.EvalSymlinks(want); err == nil {
		want = w
	}
	if h, err := filepath.EvalSymlinks(have); err == nil {
		have = h
	}
	return have != want
}

// replaceDataplane asks the running data plane to exit so a correct one can
// take its place. Restarting it drops its links, tuns and keys -- the mesh
// flaps, exactly as restarting the old single daemon did -- so it only
// happens when the running one can't be made right (wrong binary, or
// configured differently, which it cannot change live). It returns an error
// either way: the attach attempt is over, and the retry finds no data plane
// (and starts one if we own that, or waits for its supervisor to).
func (n *Node) replaceDataplane(cl *dpproto.Client, why string) error {
	n.log.Errorf("replacing the data plane: %s", why)
	if err := cl.Shutdown(); err != nil {
		cl.Close()
		return fmt.Errorf("asking the data plane to exit (%s): %w", why, err)
	}
	select {
	case <-cl.Done():
	case <-time.After(3 * time.Second):
	}
	cl.Close()
	return fmt.Errorf("replaced the data plane (%s)", why)
}

// stopDataplane is `melnode-cp -stop-dataplane`: ask a running data plane to
// exit. Stopping the control plane deliberately leaves it forwarding; this is
// how to stop the whole thing.
func stopDataplane(socket string) error {
	cl, err := dpproto.Dial(socket, nil, nil)
	if err != nil {
		return fmt.Errorf("no data plane reachable on %s: %w", socket, err)
	}
	defer cl.Close()
	if err := cl.Shutdown(); err != nil {
		return err
	}
	select {
	case <-cl.Done():
	case <-time.After(5 * time.Second):
	}
	return nil
}
