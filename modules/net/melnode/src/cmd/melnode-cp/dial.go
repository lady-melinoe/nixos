package main

import (
	"melnode/dpproto"
	"melnode/kmoddp"
)

// dialDataplane opens the data plane. Each call is a fresh start: the state a previous helper
// left in the kernel is picked up by DeviceSet, not here.
func (n *Node) dialDataplane(onPunt func(dpproto.Punt), onLiveness func(dpproto.LivenessEvent)) (dpproto.Datapath, error) {
	if n.dial != nil {
		return n.dial(onPunt, onLiveness)
	}
	return kmoddp.Open(kmoddp.Config{OnPunt: onPunt, OnLiveness: onLiveness, Logf: n.log.Verbosef})
}
