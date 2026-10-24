package main

import (
	"melnode/dpproto"
	"melnode/ebpfdp"
)

// dialDataplane opens the data plane. Each call is a fresh start: the state a previous helper
// left in the kernel is picked up by DeviceSet, not here.
func (n *Node) dialDataplane(onPunt func(dpproto.Punt)) (dpproto.Datapath, error) {
	if n.dial != nil {
		return n.dial(onPunt)
	}
	cfg := n.dpConfig
	cfg.OnPunt = onPunt
	cfg.Logf = n.log.Verbosef
	dp, err := ebpfdp.Open(cfg)
	if err != nil {
		return nil, err
	}
	return dp, nil
}
