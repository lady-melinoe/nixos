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
//	dial (starting it if it is ours) -> Hello -> DeviceSet -> sync links
//	  -> start liveness monitors -> reconcile
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

	// livenessPuntQueueSize is deliberately small: liveness (linkmonitor.go)
	// only ever has one meaningful packet in flight per peer at a time (the
	// latest state), so this only needs to smooth over brief bursts, not
	// buffer a backlog.
	livenessPuntQueueSize = 64
)

// dpSession is one attachment to the data plane.
//
// Two punt queues, not one: liveness (proto=1) is control-flow - it must
// never wait behind anything else, or a slow neighbor blows its own
// detection timer for no reason but queueing. Path-vector (proto=2) is
// data-flow: handling one packet can itself call back into the data plane
// (propagateAnnounce -> Inject, syncKernel -> RouteSet/TunCreate), which is
// exactly the kind of unbounded-latency work liveness must never sit
// behind. A single shared queue+worker used to carry both, so a burst of
// path-vector traffic (routine on every reconnect - "sending full table"
// fires for every neighbor) could starve liveness processing long enough to
// trip neighbors' own detection timers, which forces more reconnects, which
// generates more path-vector traffic: a self-sustaining flapping cascade
// (confirmed live - see arke, 2026-09-25). Splitting them by proto, each
// with its own queue and worker, is what actually fixes that: nothing
// liveness does can ever be delayed by path-vector's queue depth again.
type dpSession struct {
	cl            dpproto.Datapath
	livenessPunts chan dpproto.Punt
	punts         chan dpproto.Punt // path-vector and anything else
	done          chan struct{}     // closed to stop both punt workers
}

func (s *dpSession) onPunt(p dpproto.Punt) {
	ch := s.punts
	if p.Proto == livenessProto {
		ch = s.livenessPunts
	}
	select {
	case ch <- p:
	default: // full: drop
	}
}

// onEvent handles data plane notifications. Events are lossy hints, never
// the source of truth (a dump is), so this only logs.
func (n *Node) onEvent(e dpproto.Event) {
	if e.Kind == dpproto.EventLinkHandshake {
		n.log.Verbosef("link(%d) - handshake complete (peer at %s)", e.PeerID, e.Endpoint)
	}
}

// livenessWorker and worker are deliberately separate goroutines, not one
// goroutine select()ing on both channels: a select still dispatches one
// packet's handling to completion before it can even look at the other
// channel again, so it wouldn't actually decouple them - a slow
// path-vector handlePunt call would still delay the *next* select
// iteration from reaching an already-queued liveness packet. Two
// goroutines let each channel's handler run independently.
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

// attach connects to the data plane (starting it first if that's ours to do),
// configures it, and brings the control plane up against it.
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

	// Configure the data plane. Idempotent for an adopted data plane that is
	// already set up the same way; a different setup can't be applied live.
	dsr, err := cl.DeviceSet(n.device)
	if dpproto.IsCode(err, dpproto.CodeExists) {
		return nil, n.replaceDataplane(cl, "it was configured differently ("+err.Error()+")", false)
	}
	if err != nil {
		return fail(fmt.Errorf("configuring the data plane: %w", err))
	}
	pub := dsr.PubKey
	n.pubkey.Store(&pub)

	// Declarative link sync. Removals first: a data plane refuses a public
	// key that another peerid already uses, so a key that moved to a
	// different peerid (or two peerids that swapped keys) can only be added
	// once the old holder is gone. Then every configured link is
	// (re-)added, which also re-applies its endpoint on a data plane that
	// survived us.
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

	// From here on punts and events come to us.
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
