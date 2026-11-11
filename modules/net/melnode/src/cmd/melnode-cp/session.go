package main

import (
	"fmt"
	"time"

	"melnode/dpproto"
)

const (
	reconnectMin = time.Second
	reconnectMax = 10 * time.Second

	puntQueueSize = 1024

	livenessQueueSize = 1024

	livenessSyncInterval = time.Second
)

type dpSession struct {
	cl       dpproto.Datapath
	liveness chan dpproto.LivenessEvent
	punts    chan dpproto.Punt
	done     chan struct{}
}

func (s *dpSession) onPunt(p dpproto.Punt) {
	select {
	case s.punts <- p:
	default:
	}
}

func (s *dpSession) onLiveness(ev dpproto.LivenessEvent) {
	select {
	case s.liveness <- ev:
	default:
	}
}

func (s *dpSession) livenessWorker(n *Node) {
	for {
		select {
		case <-s.done:
			return
		case ev := <-s.liveness:
			if l := n.links[ev.Link]; l != nil {
				l.monitor.onEvent(ev)
			}
		}
	}
}

func (s *dpSession) worker(n *Node) {
	for {
		select {
		case <-s.done:
			return
		case p := <-s.punts:
			n.handlePunt(p)
		}
	}
}

// syncLiveness re-reads every link's liveness state from the data plane, in case an event was lost.
func (s *dpSession) syncLiveness(n *Node) {
	tick := time.NewTicker(livenessSyncInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		list, err := s.cl.LinkList()
		if err != nil {
			n.log.Verbosef("reading liveness: %v", err)
			continue
		}
		for _, li := range list {
			if l := n.links[li.PeerID]; l != nil {
				l.monitor.onEvent(dpproto.LivenessEvent{Link: li.PeerID, State: li.LivenessState, Diag: li.LivenessDiag})
			}
		}
	}
}

func (n *Node) runSessions(stop <-chan struct{}) {
	backoff := reconnectMin
	lastErr := ""
	for {
		select {
		case <-stop:
			return
		default:
		}

		sess, err := n.attach()
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				n.log.Errorf("data plane: %v (retrying)", err)
				lastErr = msg
			}
			select {
			case <-stop:
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > reconnectMax {
				backoff = reconnectMax
			}
			continue
		}
		backoff, lastErr = reconnectMin, ""

		select {
		case <-sess.cl.Done():
			n.log.Errorf("data plane session lost: %v", sess.cl.Err())
			n.detach(sess, true)
		case <-stop:
			n.detach(sess, false)
			return
		}
	}
}

func (n *Node) attach() (*dpSession, error) {
	sess := &dpSession{
		liveness: make(chan dpproto.LivenessEvent, livenessQueueSize),
		punts:    make(chan dpproto.Punt, puntQueueSize),
		done:     make(chan struct{}),
	}
	cl, err := n.dialDataplane(sess.onPunt, sess.onLiveness)
	if err != nil {
		return nil, err
	}
	sess.cl = cl
	fail := func(err error) (*dpSession, error) {
		cl.Close()
		return nil, err
	}

	dsr, err := cl.DeviceSet(n.device)
	if err != nil {
		return fail(fmt.Errorf("configuring the data plane: %w", err))
	}
	pub := dsr.PubKey
	n.pubkey.Store(&pub)

	for _, l := range n.sortedLinks() {
		if err := cl.LinkSet(dpproto.LinkSet{PeerID: l.id, PubKey: l.pubkey, Endpoint: l.endpoint}); err != nil {
			return fail(fmt.Errorf("configuring link %d: %w", l.id, err))
		}
	}

	go sess.livenessWorker(n)
	go sess.worker(n)
	n.dpc.Store(&dpHandle{cl})
	n.router.attach()
	for _, l := range n.sortedLinks() {
		l.monitor.Start()
	}
	go sess.syncLiveness(n)
	n.log.Verbosef("attached to data plane (node %d, mtu %d, udp port %d)", n.device.LocalID, n.device.MTU, n.device.ListenPort)
	return sess, nil
}

func (n *Node) detach(sess *dpSession, lost bool) {
	for _, l := range n.sortedLinks() {
		l.monitor.AdminDown()
		l.monitor.Stop()
	}
	n.dpc.Store(nil)
	close(sess.done)
	sess.cl.Close()
	if lost {
		n.router.detach()
	}
}
