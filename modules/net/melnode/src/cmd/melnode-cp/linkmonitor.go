package main

import (
	"sync"
	"time"

	"melnode/dpproto"
)

type linkState uint8

const (
	linkStateDown linkState = iota
	linkStateInit
	linkStateUp
	linkStateAdminDown
)

func (s linkState) String() string {
	switch s {
	case linkStateDown:
		return "Down"
	case linkStateInit:
		return "Init"
	case linkStateUp:
		return "Up"
	case linkStateAdminDown:
		return "AdminDown"
	default:
		return "Unknown"
	}
}

type diag uint8

const (
	diagNone diag = iota
	diagDetectTimeout
	diagNeighborSignaledDown
	diagAdminDown
	diagPathVectorTimeout
)

func (d diag) String() string {
	switch d {
	case diagNone:
		return "none"
	case diagDetectTimeout:
		return "detect-timeout"
	case diagNeighborSignaledDown:
		return "neighbor-signaled-down"
	case diagAdminDown:
		return "admin-down"
	case diagPathVectorTimeout:
		return "pathvector-timeout"
	default:
		return "unknown"
	}
}

const livenessProto = 1

// LinkMonitor mirrors the data plane's liveness session for one link. The eBPF data plane runs
// the protocol (sending, timers, the state machine); this keeps the last state it reported,
// passes local decisions to it, and tells the path vector when the link comes up or goes down.
type LinkMonitor struct {
	link *Link

	cbMu sync.Mutex

	mu         sync.Mutex
	running    bool
	state      linkState
	stateSince time.Time
	diag       diag
	seq        uint32

	onStateChange func(link *Link, next linkState)
}

func newLinkMonitor(link *Link) *LinkMonitor {
	return &LinkMonitor{link: link, state: linkStateDown, stateSince: time.Now()}
}

func (m *LinkMonitor) IsAlive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == linkStateUp
}

func (m *LinkMonitor) State() linkState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *LinkMonitor) SetOnStateChange(fn func(link *Link, next linkState)) {
	m.mu.Lock()
	m.onStateChange = fn
	m.mu.Unlock()
}

func (m *LinkMonitor) Start() {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	m.mu.Lock()
	m.running = true
	m.seq = 0
	m.mu.Unlock()
	m.applyLocked(linkStateDown, diagNone)
}

func (m *LinkMonitor) Stop() {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

// AdminDown tells the peer we are going away and takes the link down here at once: the data
// plane is about to be closed, so its event may never be read.
func (m *LinkMonitor) AdminDown() {
	m.force(linkStateAdminDown, diagAdminDown)
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	m.applyLocked(linkStateAdminDown, diagAdminDown)
}

// transitionTo passes a local decision, such as a path-vector timeout, to the data plane. The
// mirror follows when the data plane reports the change.
func (m *LinkMonitor) transitionTo(next linkState, d diag) {
	m.force(next, d)
}

func (m *LinkMonitor) force(next linkState, d diag) {
	cl := m.link.node.dp()
	if cl == nil {
		return
	}
	err := cl.LivenessForce(dpproto.LivenessForce{Link: m.link.id, State: uint8(next), Diag: uint8(d)})
	if err != nil {
		m.link.node.log.Verbosef("%v - liveness: setting %v: %v", m.link, next, err)
	}
}

// onEvent takes a state the data plane reported, by event or by resync. Anything older than what
// was already applied is dropped. A resync that finds changes we never saw, with the state back
// where it was, means the link flapped: replay a Down so the path vector resets its session.
func (m *LinkMonitor) onEvent(ev dpproto.LivenessEvent, resync bool) {
	if ev.State > uint8(linkStateAdminDown) {
		return
	}
	m.cbMu.Lock()
	defer m.cbMu.Unlock()
	m.mu.Lock()
	if !m.running || ev.Seq <= m.seq && !(ev.Seq == 0 && m.seq == 0) {
		m.mu.Unlock()
		return
	}
	missed := ev.Seq - m.seq
	cur := m.state
	m.seq = ev.Seq
	m.mu.Unlock()
	next := linkState(ev.State)
	if resync && missed >= 2 && next == cur && cur == linkStateUp {
		m.applyLocked(linkStateDown, diagDetectTimeout)
	}
	m.applyLocked(next, diag(ev.Diag))
}

func (m *LinkMonitor) applyLocked(next linkState, d diag) {
	m.mu.Lock()
	prev := m.state
	if prev == next {
		m.mu.Unlock()
		return
	}
	m.state = next
	m.stateSince = time.Now()
	m.diag = d
	cb := m.onStateChange
	m.mu.Unlock()

	m.link.node.log.Verbosef("%v - liveness: %v -> %v (diag=%d)", m.link, prev, next, d)

	if cb != nil {
		cb(m.link, next)
	}
}
