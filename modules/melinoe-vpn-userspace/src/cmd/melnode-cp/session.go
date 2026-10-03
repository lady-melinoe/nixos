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

	livenessPuntQueueSize = 64
)

type dpSession struct {
	cl            dpproto.Datapath
	livenessPunts chan dpproto.Punt
	punts         chan dpproto.Punt
	done          chan struct{}
}

func (s *dpSession) onPunt(p dpproto.Punt) {
	ch := s.punts
	if p.Proto == livenessProto {
		ch = s.livenessPunts
	}
	select {
	case ch <- p:
	default:
	}
}

func (s *dpSession) livenessWorker(n *Node) {
	for {
		select {
		case <-s.done:
			return
		case p := <-s.livenessPunts:
			n.handlePunt(p)
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

func (n *Node) runSessions(socket string, stop <-chan struct{}) {
	backoff := reconnectMin
	lastErr := ""
	for {
		select {
		case <-stop:
			return
		default:
		}

		sess, err := n.attach(socket)
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

func (n *Node) attach(socket string) (*dpSession, error) {
	sess := &dpSession{
		livenessPunts: make(chan dpproto.Punt, livenessPuntQueueSize),
		punts:         make(chan dpproto.Punt, puntQueueSize),
		done:          make(chan struct{}),
	}
	cl, err := n.dialDataplane(socket, sess.onPunt)
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
