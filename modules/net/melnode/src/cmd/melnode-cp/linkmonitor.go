package main

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
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

const (
	livenessProto = 1

	livenessVers1 = 1

	livenessV1Size = 24

	defaultDesiredMinTX  = 200 * time.Millisecond
	defaultRequiredMinRX = 200 * time.Millisecond
	defaultDetectMult    = 3

	// A listen-only link (no configured endpoint) only knows where its peer is from the peer's own
	// packets. Once its session has been expired this long it goes passive (RFC 5880 6.1): it stops
	// sending, which also stops the data plane's handshakes, until the peer, which dials us, makes
	// a new session, rather than probing an address the peer may have left.
	passiveAfter = 30 * time.Second

	// How long a handshake's keys are usable: WireGuard's REJECT_AFTER_TIME, the data plane's
	// t_reject_after (before TimeScale).
	sessionLifetime = 180 * time.Second
)

type livenessPacket struct {
	Diag              diag
	State             linkState
	DetectMult        uint8
	MyDiscriminator   uint32
	YourDiscriminator uint32
	DesiredMinTX      time.Duration
	RequiredMinRX     time.Duration
}

func (p *livenessPacket) encode() []byte {
	buf := make([]byte, livenessV1Size)
	buf[0] = livenessVers1
	buf[1] = byte(p.Diag)
	buf[2] = byte(p.State)
	buf[3] = p.DetectMult
	binary.BigEndian.PutUint16(buf[4:6], livenessV1Size)
	binary.BigEndian.PutUint32(buf[8:12], p.MyDiscriminator)
	binary.BigEndian.PutUint32(buf[12:16], p.YourDiscriminator)
	binary.BigEndian.PutUint32(buf[16:20], uint32(p.DesiredMinTX/time.Microsecond))
	binary.BigEndian.PutUint32(buf[20:24], uint32(p.RequiredMinRX/time.Microsecond))
	return buf
}

func decodeLivenessPacket(payload []byte) (livenessPacket, bool) {
	if len(payload) < livenessV1Size {
		return livenessPacket{}, false
	}
	return livenessPacket{
		Diag:              diag(payload[1]),
		State:             linkState(payload[2]),
		DetectMult:        payload[3],
		MyDiscriminator:   binary.BigEndian.Uint32(payload[8:12]),
		YourDiscriminator: binary.BigEndian.Uint32(payload[12:16]),
		DesiredMinTX:      time.Duration(binary.BigEndian.Uint32(payload[16:20])) * time.Microsecond,
		RequiredMinRX:     time.Duration(binary.BigEndian.Uint32(payload[20:24])) * time.Microsecond,
	}, true
}

type LinkMonitor struct {
	link *Link

	cbMu sync.Mutex

	mu                  sync.Mutex
	state               linkState
	stateSince          time.Time
	diag                diag
	localDiscriminator  uint32
	remoteDiscriminator uint32
	remoteDesiredMinTX  time.Duration
	remoteRequiredMinRX time.Duration
	sessionExpiry       time.Time

	detectTimer *time.Timer
	stopCh      chan struct{}
	wg          sync.WaitGroup

	onStateChange func(link *Link, next linkState)
}

func newLinkMonitor(link *Link) *LinkMonitor {
	var disc uint32
	for disc == 0 {
		var discBuf [4]byte
		_, _ = rand.Read(discBuf[:])
		disc = binary.BigEndian.Uint32(discBuf[:])
	}
	return &LinkMonitor{
		link:               link,
		state:              linkStateDown,
		stateSince:         time.Now(),
		localDiscriminator: disc,
	}
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

// Passive reports whether the monitor has stopped sending to wait for the peer.
func (m *LinkMonitor) Passive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.passiveLocked(time.Now())
}

func (m *LinkMonitor) passiveLocked(now time.Time) bool {
	return m.link.endpoint == "" && m.state == linkStateDown && now.Sub(m.sessionExpiry) >= passiveAfter
}

// noteSession records that the link has a session until @expiry, and answers at once if that ends
// passive mode rather than on the next tick, so the peer is not left waiting on us.
func (m *LinkMonitor) noteSession(expiry time.Time) {
	m.mu.Lock()
	if m.stopCh == nil || !expiry.After(m.sessionExpiry) {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	wasPassive := m.passiveLocked(now)
	m.sessionExpiry = expiry
	wake := wasPassive && !m.passiveLocked(now)
	m.mu.Unlock()

	if wake {
		m.link.node.log.Verbosef("%v - liveness: peer has a new session, leaving passive mode", m.link)
		m.sendPacket()
	}
}

// noteHandshake records a handshake the data plane completed at @at.
func (m *LinkMonitor) noteHandshake(at time.Time) {
	m.noteSession(at.Add(m.link.node.sessionLifetime))
}

func (m *LinkMonitor) SetOnStateChange(fn func(link *Link, next linkState)) {
	m.mu.Lock()
	m.onStateChange = fn
	m.mu.Unlock()
}

func (m *LinkMonitor) Start() {
	m.transitionTo(linkStateDown, diagNone)
	m.mu.Lock()
	m.remoteDiscriminator = 0
	// Until the data plane says otherwise, count the session as expiring now: a peer that never
	// reaches us is given up on after passiveAfter.
	m.sessionExpiry = time.Now()
	stop := make(chan struct{})
	timer := time.NewTimer(defaultRequiredMinRX * defaultDetectMult)
	m.stopCh = stop
	m.detectTimer = timer
	m.mu.Unlock()

	m.wg.Add(2)
	go m.sendLoop(stop)
	go m.detectLoop(stop, timer)
}

func (m *LinkMonitor) AdminDown() {
	m.transitionTo(linkStateAdminDown, diagAdminDown)
	m.sendPacket()
}

func (m *LinkMonitor) Stop() {
	m.mu.Lock()
	if m.stopCh == nil {
		m.mu.Unlock()
		return
	}
	close(m.stopCh)
	m.stopCh = nil
	m.detectTimer.Stop()
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *LinkMonitor) sendLoop(stop <-chan struct{}) {
	defer m.wg.Done()
	ticker := time.NewTicker(defaultDesiredMinTX)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			m.mu.Lock()
			passive := m.passiveLocked(time.Now())
			m.mu.Unlock()
			if !passive {
				m.sendPacket()
			}
		}
	}
}

func (m *LinkMonitor) detectLoop(stop <-chan struct{}, timer *time.Timer) {
	defer m.wg.Done()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			m.transitionTo(linkStateDown, diagDetectTimeout)
			m.mu.Lock()
			m.remoteDiscriminator = 0
			timer.Reset(defaultRequiredMinRX * defaultDetectMult)
			m.mu.Unlock()
		}
	}
}

func (m *LinkMonitor) sendPacket() {
	m.mu.Lock()
	pkt := livenessPacket{
		Diag:              m.diag,
		State:             m.state,
		DetectMult:        defaultDetectMult,
		MyDiscriminator:   m.localDiscriminator,
		YourDiscriminator: m.remoteDiscriminator,
		DesiredMinTX:      defaultDesiredMinTX,
		RequiredMinRX:     defaultRequiredMinRX,
	}
	m.mu.Unlock()

	m.link.send(livenessProto, linkLocalTTL, pkt.encode())
}

func (m *LinkMonitor) handlePacket(payload []byte) {
	pkt, ok := decodeLivenessPacket(payload)
	if !ok {
		m.link.node.log.Verbosef("%v - liveness: short/malformed packet, dropping", m.link)
		return
	}

	m.mu.Lock()
	if m.stopCh == nil {
		m.mu.Unlock()
		return
	}
	if pkt.MyDiscriminator == 0 ||
		(pkt.YourDiscriminator != 0 && pkt.YourDiscriminator != m.localDiscriminator) ||
		(pkt.YourDiscriminator == 0 && pkt.State != linkStateDown && pkt.State != linkStateAdminDown) {
		m.mu.Unlock()
		return
	}
	m.remoteDiscriminator = pkt.MyDiscriminator
	m.remoteDesiredMinTX = pkt.DesiredMinTX
	m.remoteRequiredMinRX = pkt.RequiredMinRX
	timeout := pkt.DesiredMinTX * time.Duration(pkt.DetectMult)
	if timeout <= 0 {
		timeout = defaultRequiredMinRX * defaultDetectMult
	}
	m.detectTimer.Reset(timeout)
	m.mu.Unlock()

	m.transition(func(cur linkState) (linkState, diag, bool) {
		if pkt.State == linkStateAdminDown {
			return linkStateDown, diagNeighborSignaledDown, true
		}
		switch cur {
		case linkStateDown, linkStateAdminDown:
			switch pkt.State {
			case linkStateDown:
				return linkStateInit, diagNone, true
			case linkStateInit:
				return linkStateUp, diagNone, true
			}
		case linkStateInit:
			if pkt.State == linkStateInit || pkt.State == linkStateUp {
				return linkStateUp, diagNone, true
			}
		case linkStateUp:
			if pkt.State == linkStateDown {
				return linkStateDown, diagNeighborSignaledDown, true
			}
		}
		return 0, 0, false
	})

	// The packet came over a session, so there is one now, whatever the next poll of the data
	// plane says.
	m.noteSession(time.Now())
}

func (m *LinkMonitor) transitionTo(next linkState, d diag) {
	m.transition(func(linkState) (linkState, diag, bool) { return next, d, true })
}

func (m *LinkMonitor) transition(decide func(cur linkState) (linkState, diag, bool)) {
	m.cbMu.Lock()
	defer m.cbMu.Unlock()

	m.mu.Lock()
	prev := m.state
	next, d, ok := decide(prev)
	if !ok || prev == next {
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
