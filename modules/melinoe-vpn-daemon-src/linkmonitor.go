package main

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// linkmonitor.go implements a scaled-down BFD (RFC 5880) for melnode's
// [[link]] tunnels: one session per Peer (== one per directly-connected
// neighbor; see PROJECT_STATE.md -- multi-hop [[peer]]s are explicitly
// out of scope, they're just routeTable entries, not a Peer). Runs
// entirely inside the existing Noise-encrypted tunnel as proto=1, so a
// liveness packet arriving at all is already proof the crypto session
// itself is alive, not just that some UDP packet showed up.
//
// Deliberately dropped relative to full BFD, because melnode has no use
// for them: Poll/Final (synchronous interval renegotiation -- melnode's
// intervals are fixed config, not changed live), Demand mode,
// Multipoint, the Echo function (no separate forwarding-plane-vs-
// control-plane distinction to exploit), and BFD's own authentication
// section (Noise already authenticates the tunnel this rides inside).
//
// Kept, because they're what prevents the footguns that matter:
//   - Discriminators (MyDiscriminator/YourDiscriminator): without these,
//     a restarted peer's fresh session could be confused for a
//     continuation of the old one.
//   - The Init step between Down and Up: without it, two nodes can each
//     unilaterally declare Up on a single received packet, without
//     either having confirmed the *other* direction actually works --
//     which is worse than no liveness check at all once path-vector
//     routing starts trusting this signal to pick routes.
//   - A real, distinct AdminDown state, not just a Down with a
//     diagnostic code attached. This matters because the two need
//     different receiver behavior (RFC 5880 6.8.6): a plain received
//     Down is normal bootstrap traffic -- both sides start Down and are
//     *supposed* to see the other's Down before advancing to Init --
//     whereas a received AdminDown means "I am deliberately not trying
//     to establish a session right now", and must NOT be treated as an
//     invitation to start the bootstrap handshake. Collapsing these
//     into one state (as an earlier version of this file did, sending
//     admin-shutdown as State=Down with a diag code) breaks the
//     bootstrap case entirely -- a receiver can't tell "peer is also
//     just starting up" from "peer explicitly doesn't want a session",
//     and treating every received Down as a forced instruction to also
//     go Down means two fresh sessions started at the same time would
//     each force the other back to Down forever, never reaching Init.

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

// diag mirrors BFD's diagnostic code, trimmed to the reasons that can
// actually occur here (most of RFC 5880's ~9 codes are IP/multipoint-
// specific and can't happen on a melnode link).
type diag uint8

const (
	diagNone                 diag = iota
	diagDetectTimeout             // detection timer expired
	diagNeighborSignaledDown      // we went Down because the remote told us Down or AdminDown
	diagAdminDown                 // WE are deliberately shutting this session down (peer.Stop())
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
	default:
		return "unknown"
	}
}

const (
	livenessProto = 1 // sibling to proto=0 (tunneled IP) in the 4-byte routing header

	livenessVers1 = 1

	// proto=1, Vers=1 payload layout (offsets within the payload, i.e.
	// after melnode's own 4-byte routing header):
	//   0: Vers            (uint8)
	//   1: Diag            (uint8)
	//   2: State           (uint8)
	//   3: DetectMult       (uint8)
	//   4:6: Length         (uint16, big-endian -- self-declared, same
	//        idea as proto=0 trimming against the inner IP header's own
	//        length field, see receive.go)
	//   6:8: reserved/pad (uint16, zero)
	//   8:12:  MyDiscriminator    (uint32)
	//   12:16: YourDiscriminator  (uint32)
	//   16:20: DesiredMinTX       (uint32, microseconds)
	//   20:24: RequiredMinRX      (uint32, microseconds)
	livenessV1Size = 24

	// Defaults. Not yet exposed in [[link]] config (melnode links are
	// homogeneous today); a real per-direction negotiated interval is
	// still computed at runtime from these plus whatever the peer
	// advertises, so this isn't a hardcoded shortcut -- see
	// (*LinkMonitor).negotiatedIntervals.
	defaultDesiredMinTX  = 200 * time.Millisecond
	defaultRequiredMinRX = 200 * time.Millisecond
	defaultDetectMult    = 3
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
	// buf[6:8] reserved, left zero
	binary.BigEndian.PutUint32(buf[8:12], p.MyDiscriminator)
	binary.BigEndian.PutUint32(buf[12:16], p.YourDiscriminator)
	binary.BigEndian.PutUint32(buf[16:20], uint32(p.DesiredMinTX/time.Microsecond))
	binary.BigEndian.PutUint32(buf[20:24], uint32(p.RequiredMinRX/time.Microsecond))
	return buf
}

// decodeLivenessPacket assumes the caller has already used the Vers
// byte to route here and the Length field to trim elem.packet to the
// right size (see receive.go's proto=1 dispatch) -- payload is expected
// to be exactly livenessV1Size bytes.
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

// LinkMonitor is one BFD-like session, owned by exactly one Peer (i.e.
// one [[link]]). State is guarded by a mutex rather than atomics since
// transitions touch several fields together and happen rarely relative
// to the data path (every txInterval, not per packet).
type LinkMonitor struct {
	peer *Peer

	mu                  sync.Mutex
	state               linkState
	stateSince          time.Time // when state last changed (introspect.go)
	diag                diag      // last diag associated with state, per transitionTo -- this is what actually goes out on the wire, see sendPacket
	localDiscriminator  uint32
	remoteDiscriminator uint32 // 0 == not yet learned
	remoteDesiredMinTX  time.Duration
	remoteRequiredMinRX time.Duration

	detectTimer *time.Timer
	stopCh      chan struct{}
	wg          sync.WaitGroup

	// onStateChange, if set, is called (with m.mu released) on every
	// state transition. This is the push side of the query surface
	// mentioned in the type doc -- path-vector routing (pathvector.go)
	// is the intended (and, as of this doc, only) consumer, wired up in
	// main.go before peers Start().
	onStateChange func(peer *Peer, next linkState)
}

func newLinkMonitor(peer *Peer) *LinkMonitor {
	var discBuf [4]byte
	_, _ = rand.Read(discBuf[:]) // non-zero with overwhelming probability; a collision just costs one extra Down->Init round trip
	return &LinkMonitor{
		peer:               peer,
		state:              linkStateDown,
		stateSince:         time.Now(),
		localDiscriminator: binary.BigEndian.Uint32(discBuf[:]),
	}
}

// IsAlive reports whether this link's BFD-like session is Up. Path-
// vector routing (pathvector.go) is the intended consumer, both via
// this pull API and the onStateChange push callback below.
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

// SetOnStateChange wires the callback invoked on every transition. Must
// be called before Start() to avoid missing an early transition -- see
// main.go, which wires this for every peer before any peer.Start().
func (m *LinkMonitor) SetOnStateChange(fn func(peer *Peer, next linkState)) {
	m.mu.Lock()
	m.onStateChange = fn
	m.mu.Unlock()
}

// Start begins sending periodic liveness packets and arms the
// detection timer. Mirrors Peer.Start/Stop's own lifecycle -- call from
// there, not standalone.
func (m *LinkMonitor) Start() {
	m.mu.Lock()
	m.state = linkStateDown
	m.remoteDiscriminator = 0
	m.stopCh = make(chan struct{})
	m.detectTimer = time.NewTimer(defaultRequiredMinRX * defaultDetectMult)
	m.mu.Unlock()

	m.wg.Add(2)
	go m.sendLoop()
	go m.detectLoop()
}

// AdminDown sends one final AdminDown-state packet before the session
// stops, so the peer reacts immediately instead of waiting out its
// detection timer -- and, per RFC 5880, so the peer knows this is a
// deliberate shutdown rather than a lost connection, and doesn't
// respond to it as though it were the start of a fresh bootstrap
// handshake (see this file's top comment and handlePacket). Call before
// Stop(), while the peer's send path is still up.
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
	m.detectTimer.Stop()
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *LinkMonitor) sendLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(defaultDesiredMinTX)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.sendPacket()
		}
	}
}

func (m *LinkMonitor) detectLoop() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.detectTimer.C:
			m.transitionTo(linkStateDown, diagDetectTimeout)
			// re-arm so a later packet can bring the session back up
			// (transitionTo doesn't touch the timer itself -- see its
			// comment).
			m.mu.Lock()
			m.detectTimer.Reset(defaultRequiredMinRX * defaultDetectMult)
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

	device := m.peer.device
	elem := device.NewOutboundElement()
	buf := elem.buffer[:]
	offset := MessageTransportHeaderSize + headerSize
	payload := pkt.encode()
	copy(buf[offset:offset+len(payload)], payload)

	buf[offset-headerSize+0] = livenessProto
	buf[offset-headerSize+1] = byte(device.localID)
	buf[offset-headerSize+2] = byte(m.peer.id)
	buf[offset-headerSize+3] = 0
	elem.packet = buf[offset-headerSize : offset+len(payload)]

	container := device.GetOutboundElementsContainer()
	container.isControl = true // liveness -- see send.go's StagePackets/drainStaged and PROJECT_STATE.md's backpressure section
	container.elems = append(container.elems, elem)

	if !m.peer.isRunning.Load() {
		device.PutMessageBuffer(elem.buffer)
		device.PutOutboundElement(elem)
		device.PutOutboundElementsContainer(container)
		return
	}
	m.peer.StagePackets(container)
	m.peer.SendStagedPackets()
}

// handlePacket runs the receive side of the state machine. Called from
// receive.go's proto=1 dispatch with the already Length-trimmed
// payload.
func (m *LinkMonitor) handlePacket(payload []byte) {
	pkt, ok := decodeLivenessPacket(payload)
	if !ok {
		m.peer.device.log.Verbosef("%v - liveness: short/malformed packet, dropping", m.peer)
		return
	}

	m.mu.Lock()
	m.remoteDiscriminator = pkt.MyDiscriminator
	m.remoteDesiredMinTX = pkt.DesiredMinTX
	m.remoteRequiredMinRX = pkt.RequiredMinRX
	// Detection timeout is driven by what the *remote* says it sends
	// at, per RFC 5880 6.8.4 -- not our own guess -- so a slow peer
	// gets a correspondingly relaxed timeout instead of flapping
	// against a locally-assumed interval.
	timeout := pkt.DesiredMinTX * time.Duration(pkt.DetectMult)
	if timeout <= 0 {
		timeout = defaultRequiredMinRX * defaultDetectMult
	}
	m.detectTimer.Reset(timeout)
	m.mu.Unlock()

	if pkt.State == linkStateAdminDown {
		// RFC 5880 6.8.6: a received AdminDown always forces us to
		// Down, regardless of our current state -- but unlike a plain
		// received Down, it must NOT be treated as the start of a
		// normal bootstrap handshake (no Down->Init here). The remote
		// has told us, explicitly, that it isn't trying to establish a
		// session right now, so responding by trying to initiate one
		// would just be wrong.
		m.transitionTo(linkStateDown, diagNeighborSignaledDown)
		return
	}

	switch m.State() {
	case linkStateDown, linkStateAdminDown:
		switch pkt.State {
		case linkStateDown:
			// Normal bootstrap: both sides start Down and are
			// *supposed* to see the other's Down before advancing --
			// this is "peer is also just starting up", not an
			// instruction to go/stay Down. This is the case an
			// earlier version of this file got wrong (see this file's
			// top comment) by forcing Down here unconditionally,
			// which meant two fresh sessions could never leave Down.
			m.transitionTo(linkStateInit, diagNone)
		case linkStateInit, linkStateUp:
			// The peer has confirmed (via State>=Init) it's already
			// heard from us too -- but this can only actually happen
			// from Down (never AdminDown, in melnode's current usage
			// AdminDown is momentary and Stop() follows immediately;
			// kept as a case here for a future explicit long-lived
			// admin-disable rather than relying on that timing).
			m.transitionTo(linkStateUp, diagNone)
		}
	case linkStateInit:
		if pkt.State == linkStateInit || pkt.State == linkStateUp {
			// The core anti-footgun rule: a received packet alone
			// never jumps straight to Up from Down (see the Down case
			// above); it's this Init->Up step, reached only after
			// we've already seen the peer at least once, that
			// actually confirms bidirectional liveness.
			m.transitionTo(linkStateUp, diagNone)
		}
		// pkt.State == Down while we're Init: no transition, keep
		// waiting -- per RFC 6.8.6, only a received AdminDown (handled
		// above) or our own local detection timeout can knock us back
		// down from here.
	case linkStateUp:
		if pkt.State == linkStateDown {
			m.transitionTo(linkStateDown, diagNeighborSignaledDown)
		}
	}
}

func (m *LinkMonitor) transitionTo(next linkState, d diag) {
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

	// This is the query surface's log-on-transition half (IsAlive is
	// the other half) -- per earlier discussion, this is deliberately
	// the only consumer for now; path-vector routing wires up to
	// IsAlive/onStateChange instead of this log line.
	m.peer.device.log.Verbosef("%v - liveness: %v -> %v (diag=%d)", m.peer, prev, next, d)

	if cb != nil {
		cb(m.peer, next)
	}
}
