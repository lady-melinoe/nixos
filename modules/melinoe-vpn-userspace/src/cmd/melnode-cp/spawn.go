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

const spawnWait = 5 * time.Second

func (n *Node) dialDataplane(socket string, onPunt func(dpproto.Punt), onEvent func(dpproto.Event)) (dpproto.Datapath, error) {
	if n.kernelDataplane {
		return dpproto.DialKernel(onPunt, onEvent)
	}
	cl, err := dpproto.Dial(socket, onPunt, onEvent)
	if err == nil {
		return cl, nil
	}
	if len(n.dpCommand) == 0 {
		return nil, err
	}
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

func (n *Node) spawnDataplane(socket string) error {
	n.childMu.Lock()
	defer n.childMu.Unlock()
	if n.child != nil {
		select {
		case <-n.child:
		default:
			return nil
		}
	}

	argv := n.dpCommand
	args := append(append([]string(nil), argv[1:]...), "-socket", socket)
	cmd := exec.Command(argv[0], args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	n.log.Verbosef("started data plane %v (pid %d)", argv, cmd.Process.Pid)

	done := make(chan struct{})
	n.child = done
	go func() {
		err := cmd.Wait()
		n.log.Errorf("data plane (pid %d) exited: %v", cmd.Process.Pid, err)
		close(done)
	}()
	return nil
}

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

func (n *Node) replaceDataplane(cl dpproto.Datapath, why string, restart bool) error {
	n.log.Errorf("replacing the data plane: %s", why)
	if err := cl.DeviceDel(); err != nil {
		cl.Close()
		return fmt.Errorf("tearing down the data plane's device (%s): %w", why, err)
	}
	if q, ok := cl.(dpproto.Quitter); ok && restart {
		if err := q.Quit(); err != nil {
			cl.Close()
			return fmt.Errorf("asking the data plane to exit (%s): %w", why, err)
		}
		select {
		case <-cl.Done():
		case <-time.After(3 * time.Second):
		}
	}
	cl.Close()
	return fmt.Errorf("replaced the data plane (%s)", why)
}

func stopDataplane(cfg *Config) error {
	var cl dpproto.Datapath
	var err error
	if cfg.KernelDataplane {
		cl, err = dpproto.DialKernel(nil, nil)
	} else {
		cl, err = dpproto.Dial(cfg.DataplaneSocket, nil, nil)
	}
	if err != nil {
		return fmt.Errorf("no data plane reachable: %w", err)
	}
	defer cl.Close()
	if err := cl.DeviceDel(); err != nil {
		return err
	}
	if q, ok := cl.(dpproto.Quitter); ok {
		if err := q.Quit(); err != nil {
			return err
		}
	}
	select {
	case <-cl.Done():
	case <-time.After(5 * time.Second):
	}
	return nil
}
