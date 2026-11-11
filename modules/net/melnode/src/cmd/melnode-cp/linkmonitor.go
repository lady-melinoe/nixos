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
	m.mu.Lock()
	m.running = true
	m.mu.Unlock()
	m.apply(linkStateDown, diagNone)
}

func (m *LinkMonitor) Stop() {
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

func (m *LinkMonitor) AdminDown() {
	m.force(linkStateAdminDown, diagAdminDown)
	m.apply(linkStateAdminDown, diagAdminDown)
}

// transitionTo applies a local decision, such as a path-vector timeout, to the data plane's
// session and to this mirror.
func (m *LinkMonitor) transitionTo(next linkState, d diag) {
	m.force(next, d)
	m.apply(next, d)
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

// onEvent takes a state change the data plane reported.
func (m *LinkMonitor) onEvent(ev dpproto.LivenessEvent) {
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()
	if running {
		m.apply(linkState(ev.State), diag(ev.Diag))
	}
}

func (m *LinkMonitor) apply(next linkState, d diag) {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()

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
