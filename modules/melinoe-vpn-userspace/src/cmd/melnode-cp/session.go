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

func (n *Node) onEvent(e dpproto.Event) {
	if e.Kind == dpproto.EventLinkHandshake {
		n.log.Verbosef("link(%d) - handshake complete (peer at %s)", e.PeerID, e.Endpoint)
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
	cl, err := n.dialDataplane(socket, sess.onPunt, n.onEvent)
	if err != nil {
		return nil, err
	}
	sess.cl = cl
	fail := func(err error) (*dpSession, error) {
		cl.Close()
		return nil, err
	}

	hr, err := cl.Hello()
	if err != nil {
		return fail(fmt.Errorf("hello: %w", err))
	}
	if n.wrongBinary(hr.PID) {
		return nil, n.replaceDataplane(cl, fmt.Sprintf("pid %d is not running %s", hr.PID, n.dpCommand[0]), true)
	}

	dsr, err := cl.DeviceSet(n.device)
	if dpproto.IsCode(err, dpproto.CodeExists) {
		return nil, n.replaceDataplane(cl, "it was configured differently ("+err.Error()+")", false)
	}
	if err != nil {
		return fail(fmt.Errorf("configuring the data plane: %w", err))
	}
	pub := dsr.PubKey
	n.pubkey.Store(&pub)

	want := n.links
	have, err := cl.LinkList()
	if err != nil {
		return fail(fmt.Errorf("listing links: %w", err))
	}
	for _, li := range have {
		if l, ok := want[li.PeerID]; ok && l.pubkey == li.PubKey {
			continue
		}
		if err := cl.LinkDel(li.PeerID); err != nil && !dpproto.IsCode(err, dpproto.CodeNotFound) {
			return fail(fmt.Errorf("removing stale link %d: %w", li.PeerID, err))
		}
		n.log.Verbosef("removed link %d from the data plane (not in config, or its key changed)", li.PeerID)
	}
	for _, l := range n.sortedLinks() {
		if err := cl.LinkAdd(dpproto.LinkAdd{PeerID: l.id, PubKey: l.pubkey, Endpoint: l.endpoint}); err != nil {
			return fail(fmt.Errorf("configuring link %d: %w", l.id, err))
		}
	}

	if err := cl.Attach(); err != nil {
		return fail(fmt.Errorf("attaching to the data plane: %w", err))
	}
	go sess.livenessWorker(n)
	go sess.worker(n)
	n.dpc.Store(&dpHandle{cl})
	n.router.attach()
	for _, l := range n.sortedLinks() {
		l.monitor.Start()
	}
	n.log.Verbosef("attached to data plane (pid %d, node %d, mtu %d, udp port %d)", hr.PID, n.device.LocalID, n.device.MTU, n.device.ListenPort)
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
