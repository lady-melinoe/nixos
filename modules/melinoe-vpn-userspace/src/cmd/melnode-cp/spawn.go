package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"melnode/dpproto"
)

const spawnWait = 5 * time.Second

func (n *Node) dialDataplane(socket string, onPunt func(dpproto.Punt)) (dpproto.Datapath, error) {
	if n.kernelDataplane {
		return dpproto.DialKernel(onPunt)
	}
	cl, err := dpproto.Dial(socket, onPunt)
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
		if cl, err = dpproto.Dial(socket, onPunt); err == nil {
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
