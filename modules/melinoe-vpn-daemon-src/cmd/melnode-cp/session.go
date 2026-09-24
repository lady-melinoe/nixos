package main

import (
	"fmt"
	"time"

	"melnode/dpproto"
)

// session.go: attaching to the data plane, keeping it programmed, and
// noticing when it goes away.
//
// The data plane is the durable half: it keeps forwarding with whatever it
// was last given if we die or restart, and we reconcile against whatever we
// find when we (re)attach. A session is therefore:
//
//	dial -> Hello -> sync links -> start liveness monitors -> reconcile
//
// and ends when the socket drops (data plane went away) or we are told to
// stop. Everything path-vector learned rides on link liveness, so ending a
// session takes every link down, withdrawing all routes, exactly as if the
// links themselves had failed -- which, from the mesh's point of view, they
// have.

const (
	reconnectMin = time.Second
	reconnectMax = 10 * time.Second

	// puntQueueSize bounds control packets waiting for handlePunt. Handling
	// runs on its own goroutine (not the socket reader) so a slow handler can
	// never delay RPC replies; overflow is dropped, which liveness and
	// path-vector tolerate.
	puntQueueSize = 1024
)

// dpSession is one attachment to the data plane.
type dpSession struct {
	cl    *dpproto.Client
	punts chan dpproto.Punt
	done  chan struct{} // closed to stop the punt worker
}

func (s *dpSession) onPunt(p dpproto.Punt) {
	select {
	case s.punts <- p:
	default: // full: drop
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

// runSessions keeps a session attached until stop is closed. On stop it takes
// the links down gracefully (AdminDown to each neighbor, so they react at once
// instead of waiting out a detection timer) and returns WITHOUT touching the
// data plane's links, tuns or routes: they keep forwarding.
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
			// Don't repeat the same complaint every retry.
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

// attach dials the data plane and brings the control plane up against it.
func (n *Node) attach(socket string) (*dpSession, error) {
	sess := &dpSession{punts: make(chan dpproto.Punt, puntQueueSize), done: make(chan struct{})}
	cl, err := dpproto.Dial(socket, sess.onPunt)
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
	if hr.LocalID != n.localID {
		return fail(fmt.Errorf("data plane is node %d but this control plane is configured as node %d", hr.LocalID, n.localID))
	}
	n.mtu.Store(int32(hr.MTU))
	n.hello.Store(&hr)

	// Declarative link sync: every configured link is (re-)added, which also
	// re-applies its endpoint on a data plane that survived us; anything
	// else the data plane has is removed.
	want := n.links
	for _, l := range n.sortedLinks() {
		err := cl.LinkAdd(dpproto.LinkAdd{PeerID: l.id, PubKey: l.pubkey, Endpoint: l.endpoint})
		if dpproto.IsCode(err, dpproto.CodeExists) {
			// Same peerid, different key: the config changed. Replace it.
			if err = cl.LinkDel(l.id); err == nil {
				err = cl.LinkAdd(dpproto.LinkAdd{PeerID: l.id, PubKey: l.pubkey, Endpoint: l.endpoint})
			}
		}
		if err != nil {
			return fail(fmt.Errorf("configuring link %d: %w", l.id, err))
		}
	}
	have, err := cl.LinkList()
	if err != nil {
		return fail(fmt.Errorf("listing links: %w", err))
	}
	for _, li := range have {
		if _, ok := want[li.PeerID]; ok {
			continue
		}
		if err := cl.LinkDel(li.PeerID); err != nil && !dpproto.IsCode(err, dpproto.CodeNotFound) {
			return fail(fmt.Errorf("removing stale link %d: %w", li.PeerID, err))
		}
		n.log.Verbosef("removed link %d from the data plane (not in config)", li.PeerID)
	}

	go sess.worker(n)
	n.dpc.Store(cl)
	n.router.attach()
	for _, l := range n.sortedLinks() {
		l.monitor.Start()
	}
	n.log.Verbosef("attached to data plane (node %d, mtu %d, udp port %d)", hr.LocalID, hr.MTU, hr.Port)
	return sess, nil
}

// detach ends a session. lost is true when the data plane went away (as
// opposed to us shutting down on purpose): then the tuns it had are gone or
// unknown, so the host-side hooks are undone too.
func (n *Node) detach(sess *dpSession, lost bool) {
	// Links first, while the session is still (if it is) usable: AdminDown
	// sends one last liveness packet so neighbors learn of a deliberate
	// shutdown immediately. On a lost session those sends just fail fast.
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
