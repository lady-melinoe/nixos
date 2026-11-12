// Package ebpfdp is the data plane: it has the melnode eBPF object loaded and attached by the
// loader (mn-up load), takes over its link, maps and SYSCALL programs and removes the pins, and
// implements dpproto.Datapath on top of it.
//
// The process holding the link owns the data plane: when it exits, however it exits, the kernel
// closes the link and the device, sockets and sessions go with it, so peers see the node go down.
package ebpfdp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"melnode/dpproto"
)

const (
	DefaultPinDir = "/sys/fs/bpf/melnode"

	maxPeers    = 256
	afINET      = 2
	afINET6     = 10
	injectMax   = 4096
	timeScale1  = 1
	nsPerSec    = 1000000000
	updEndpoint = 2
)

// Config says where the object and its loader are and how it is run.
type Config struct {
	ObjectPath string // melnode.bpf.o
	// KeepPinned leaves the data plane pinned under PinDir, for tools that exit after setting it up.
	KeepPinned bool
	// LoaderPath is the mn-up binary; default $MELNODE_LOADER, else mn-up on PATH.
	LoaderPath string
	PinDir     string // default DefaultPinDir
	// TimeScale divides the protocol timers (tests only); 0 or 1 is real time.
	TimeScale uint64
	// UnderLoadThreshold is not supported by the v2 object, which decides load like kernel
	// WireGuard does; a nonzero value is logged and ignored.
	UnderLoadThreshold uint32
	// Variables sets named `const volatile` variables of the object before it is loaded.
	Variables map[string]uint64
	// OnPunt receives control-plane packets, from its own goroutine; it must not block.
	OnPunt func(dpproto.Punt)
	// OnLiveness receives link liveness changes, from its own goroutine; it must not block.
	OnLiveness func(dpproto.LivenessEvent)
	Logf       func(format string, args ...any)
}

type layouts struct {
	dev, peer, ep, addr, setup, peerArgs, peerUpd, route, inject, punt, bfd, bfdArgs, bfdEvent *layout
}

// DP is a data plane. Create it with Open and configure it with DeviceSet.
type DP struct {
	cfg  Config
	spec *ebpf.CollectionSpec
	lay  layouts

	statNames []string

	mu     sync.Mutex
	isSet  bool
	devCfg dpproto.DeviceSet
	pub    [32]byte
	maps   map[string]*ebpf.Map
	progs  map[string]*ebpf.Program
	rd     *ringbuf.Reader
	erd    *ringbuf.Reader
	owner  link.Link

	done    chan struct{}
	errOnce sync.Once
	err     error
}

var _ dpproto.Datapath = (*DP)(nil)

var mapNames = []string{
	"wg_dev", "wg_peers", "wg_peer_by_pub", "routes",
	"punts", "stats", "mn_bfd", "mn_events",
	"mn_tm_bfd", "wg_gone", "wg_gc",
	"wg_tm_retrans", "wg_tm_keepal", "wg_tm_newhs",
	"wg_tm_zero", "wg_tm_persist", "wg_tm_hswork",
}

var progNames = []string{
	"wg_set_identity", "wg_setup", "wg_teardown", "wg_peer_prepare", "wg_peer_update",
	"wg_peer_remove", "mn_route_set", "mn_inject", "mn_bfd_force",
}

// Open reads the object and checks it is the one this helper understands. It touches no kernel
// state.
func Open(cfg Config) (*DP, error) {
	if cfg.ObjectPath == "" {
		return nil, errors.New("ebpfdp: no object path")
	}
	if cfg.PinDir == "" {
		cfg.PinDir = DefaultPinDir
	}
	if cfg.LoaderPath == "" {
		cfg.LoaderPath = os.Getenv("MELNODE_LOADER")
	}
	if cfg.LoaderPath == "" {
		cfg.LoaderPath = "mn-up"
	}
	if cfg.TimeScale == 0 {
		cfg.TimeScale = timeScale1
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	spec, err := ebpf.LoadCollectionSpec(cfg.ObjectPath)
	if err != nil {
		return nil, fmt.Errorf("ebpfdp: reading %s: %w", cfg.ObjectPath, err)
	}
	d := &DP{cfg: cfg, spec: spec, done: make(chan struct{})}
	if err := d.checkSpec(); err != nil {
		return nil, fmt.Errorf("ebpfdp: %s: %w", cfg.ObjectPath, err)
	}
	return d, nil
}

func (d *DP) checkSpec() error {
	for _, n := range mapNames {
		if d.spec.Maps[n] == nil {
			return fmt.Errorf("no map %q", n)
		}
	}
	for _, n := range progNames {
		if d.spec.Programs[n] == nil {
			return fmt.Errorf("no program %q", n)
		}
	}
	if d.spec.Variables["mn_psec"] == nil {
		return errors.New("no variable mn_psec")
	}
	types := d.spec.Types
	var err error
	mk := func(name string, want map[string]int) *layout {
		if err != nil {
			return nil
		}
		var l *layout
		l, err = newLayout(types, name, want)
		return l
	}
	d.lay.addr = mk("bpfx_addr", map[string]int{"family": 2, "port": 2, "addr": 16})
	d.lay.ep = mk("wg_endpoint", map[string]int{"addr": 0})
	d.lay.dev = mk("wg_dev", map[string]int{"port": 2, "local_id": 2, "mtu": 4, "fwmark": 4, "name": 16, "priv": 32, "pub": 32})
	d.lay.peer = mk("wg_peer", map[string]int{"present": 4, "pub": 32, "ep": 0, "walltime_last_handshake": 8, "rx_bytes": 8, "tx_bytes": 8})
	d.lay.setup = mk("wg_setup_args", map[string]int{"name": 16, "mtu": 4, "fwmark": 4, "port": 2, "local_id": 2, "err": 4})
	d.lay.peerArgs = mk("wg_peer_args", map[string]int{"slot": 4, "err": 4})
	d.lay.peerUpd = mk("wg_peer_upd", map[string]int{"slot": 4, "err": 4, "flags": 4, "ep": 0})
	d.lay.route = mk("mn_route_args", map[string]int{"dst": 4, "nexthop": 4, "present": 4, "err": 4})
	d.lay.inject = mk("mn_inject_args", map[string]int{"link": 4, "len": 4, "proto": 1, "dst": 1, "ttl": 1, "err": 4})
	d.lay.punt = mk("mn_punt_hdr", map[string]int{"link": 4, "len": 4, "proto": 1, "src": 1, "dst": 1, "ttl": 1})
	d.lay.bfd = mk("mn_bfd", map[string]int{"state": 4, "diag": 4, "passive": 4, "seq": 4, "local_disc": 4, "remote_disc": 4, "remote_tx_us": 4, "remote_rx_us": 4})
	d.lay.bfdArgs = mk("mn_bfd_args", map[string]int{"slot": 4, "err": 4, "state": 4, "diag": 4})
	d.lay.bfdEvent = mk("mn_bfd_event", map[string]int{"link": 4, "state": 4, "diag": 4, "seq": 4})
	if err != nil {
		return err
	}
	var enum *btf.Enum
	if err := types.TypeByName("mn_stat_id", &enum); err != nil {
		return fmt.Errorf("no enum mn_stat_id: %w", err)
	}
	for _, v := range enum.Values {
		if n := strings.TrimPrefix(v.Name, "MN_ST_"); n != "MAX" {
			d.statNames = append(d.statNames, n)
		}
	}
	registerStatNames(d.statNames)
	return nil
}

func (d *DP) Done() <-chan struct{} { return d.done }

func (d *DP) Err() error {
	select {
	case <-d.done:
		return d.err
	default:
		return nil
	}
}

func (d *DP) fail(err error) {
	d.errOnce.Do(func() {
		d.err = err
		close(d.done)
	})
}

// DeviceSet creates the data plane's device and sockets, after tearing down any an earlier run
// left pinned. Called again with the same configuration it returns the public key; with another
// it fails.
func (d *DP) DeviceSet(m dpproto.DeviceSet) (dpproto.DeviceSetReply, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m.LocalID == 0 || m.LocalID > 255 {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "local id %d out of range 1-255", m.LocalID)
	}
	if m.ListenPort == 0 {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "listen port must be set")
	}
	if m.MTU < 576 || m.MTU > 65000 {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "mtu %d out of range", m.MTU)
	}
	if m.PrivateKey == ([32]byte{}) {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "private key is zero")
	}
	if m.IfName == "" || len(m.IfName) > 15 || strings.ContainsAny(m.IfName, "/ \t\n%") {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "device name %q must be 1-15 characters with no spaces, slashes or %%", m.IfName)
	}
	if d.isSet {
		if m != d.devCfg {
			return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeExists, "already configured differently")
		}
		return dpproto.DeviceSetReply{PubKey: d.pub}, nil
	}
	if d.pinnedExists() {
		d.cfg.Logf("ebpfdp: removing the state an earlier run left in %s", d.cfg.PinDir)
		d.discardPinned()
	}
	if err := d.create(m); err != nil {
		return dpproto.DeviceSetReply{}, err
	}
	return dpproto.DeviceSetReply{PubKey: d.pub}, nil
}

func (d *DP) pinnedExists() bool {
	_, err := os.Stat(d.cfg.PinDir)
	return err == nil
}

func (d *DP) unpinAll() {
	if err := os.RemoveAll(d.cfg.PinDir); err != nil {
		d.cfg.Logf("ebpfdp: removing %s: %v", d.cfg.PinDir, err)
	}
}

func (d *DP) load() error {
	if err := os.MkdirAll(filepath.Dir(d.cfg.PinDir), 0o755); err != nil {
		return fmt.Errorf("ebpfdp: %w", err)
	}
	args := []string{"load", d.cfg.ObjectPath, d.cfg.PinDir,
		"--set", fmt.Sprintf("mn_psec=%d", nsPerSec/d.cfg.TimeScale)}
	for name, v := range d.cfg.Variables {
		args = append(args, "--set", fmt.Sprintf("%s=%d", name, v))
	}
	if d.cfg.UnderLoadThreshold != 0 {
		d.cfg.Logf("ebpfdp: the v2 object has no load threshold; ignoring %d", d.cfg.UnderLoadThreshold)
	}
	out, err := exec.Command(d.cfg.LoaderPath, args...).CombinedOutput()
	if err != nil {
		d.unpinAll()
		return fmt.Errorf("ebpfdp: loading the object: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *DP) openPinned() error {
	maps := make(map[string]*ebpf.Map, len(mapNames))
	progs := make(map[string]*ebpf.Program, len(progNames))
	fail := func(err error) error {
		for _, mp := range maps {
			mp.Close()
		}
		for _, p := range progs {
			p.Close()
		}
		return err
	}
	for _, n := range mapNames {
		mp, err := ebpf.LoadPinnedMap(filepath.Join(d.cfg.PinDir, "maps", n), nil)
		if err != nil {
			return fail(fmt.Errorf("ebpfdp: map %s: %w", n, err))
		}
		maps[n] = mp
	}
	for _, n := range progNames {
		p, err := ebpf.LoadPinnedProgram(filepath.Join(d.cfg.PinDir, "progs", n), nil)
		if err != nil {
			return fail(fmt.Errorf("ebpfdp: program %s: %w", n, err))
		}
		progs[n] = p
	}
	d.maps, d.progs = maps, progs
	return nil
}

func (d *DP) create(m dpproto.DeviceSet) error {
	if err := d.load(); err != nil {
		return err
	}
	if err := d.openPinned(); err != nil {
		d.unpinAll()
		return err
	}
	if !d.cfg.KeepPinned {
		owner, err := link.LoadPinnedLink(filepath.Join(d.cfg.PinDir, "link"), nil)
		if err != nil {
			d.closeHandles()
			d.unpinAll()
			return fmt.Errorf("ebpfdp: opening the data plane link: %w", err)
		}
		d.owner = owner
		d.unpinAll()
	}
	cleanup := func(err error) error {
		d.teardownLocked()
		return err
	}
	dev := d.lay.dev.new()
	copy(d.lay.dev.bytes(dev, "priv"), m.PrivateKey[:])
	if err := d.maps["wg_dev"].Update(uint32(0), dev, ebpf.UpdateAny); err != nil {
		return cleanup(fmt.Errorf("ebpfdp: writing the device: %w", err))
	}
	if ret, _, err := d.runSyscall("wg_set_identity", d.lay.peerArgs.new()); err != nil || ret != 0 {
		return cleanup(fmt.Errorf("ebpfdp: setting the identity: ret %d: %v", int32(ret), err))
	}
	ctx := d.lay.setup.new()
	copy(d.lay.setup.bytes(ctx, "name"), m.IfName)
	d.lay.setup.setU32(ctx, "mtu", m.MTU)
	d.lay.setup.setU32(ctx, "fwmark", m.Fwmark)
	d.lay.setup.setU16(ctx, "port", m.ListenPort)
	d.lay.setup.setU16(ctx, "local_id", uint16(m.LocalID))
	ret, _, err := d.runSyscall("wg_setup", ctx)
	if err != nil {
		return cleanup(fmt.Errorf("ebpfdp: running wg_setup: %w", err))
	}
	if errno := int32(ret); errno != 0 {
		return cleanup(dpproto.Errorf(codeFor(errno), "creating device %s failed: %v", m.IfName, unix.Errno(-errno)))
	}
	if err := d.maps["wg_dev"].Lookup(uint32(0), dev); err != nil {
		return cleanup(fmt.Errorf("ebpfdp: reading the device: %w", err))
	}
	copy(d.pub[:], d.lay.dev.bytes(dev, "pub"))
	if err := d.startPunts(); err != nil {
		return cleanup(err)
	}
	d.isSet, d.devCfg = true, m
	return nil
}

// loadPinned opens the pinned maps and programs and reads the node's configuration from them.
func (d *DP) loadPinned() (cfg dpproto.DeviceSet, pub [32]byte, ok bool) {
	if err := d.openPinned(); err != nil {
		return cfg, pub, false
	}
	dev := d.lay.dev.new()
	if err := d.maps["wg_dev"].Lookup(uint32(0), dev); err != nil {
		d.closeHandles()
		return cfg, pub, false
	}
	cfg = dpproto.DeviceSet{
		LocalID:    uint32(d.lay.dev.getU16(dev, "local_id")),
		ListenPort: d.lay.dev.getU16(dev, "port"),
		Fwmark:     d.lay.dev.getU32(dev, "fwmark"),
		MTU:        d.lay.dev.getU32(dev, "mtu"),
		IfName:     strings.TrimRight(string(d.lay.dev.bytes(dev, "name")), "\x00"),
	}
	copy(cfg.PrivateKey[:], d.lay.dev.bytes(dev, "priv"))
	copy(pub[:], d.lay.dev.bytes(dev, "pub"))
	return cfg, pub, true
}

// discardPinned removes the state an earlier run left pinned: through its own teardown program if
// the pins still load, otherwise just the pins.
func (d *DP) discardPinned() {
	if _, _, ok := d.loadPinned(); ok {
		d.teardownLocked()
		return
	}
	d.unpinAll()
}

// Attach opens the state a running data plane left pinned, whatever its configuration, for
// tools that inspect or change it. It does not read control-plane packets (see StartPunts) and
// Detach leaves the data plane running.
func (d *DP) Attach() (dpproto.DeviceSet, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.isSet {
		return d.devCfg, nil
	}
	cfg, pub, ok := d.loadPinned()
	if !ok {
		return dpproto.DeviceSet{}, dpproto.Errorf(dpproto.CodeNotReady, "no data plane in %s", d.cfg.PinDir)
	}
	d.pub = pub
	d.isSet, d.devCfg = true, cfg
	return cfg, nil
}

// StartPunts begins delivering control-plane packets to Config.OnPunt (only one reader should
// do this at a time).
func (d *DP) StartPunts() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	if d.rd != nil {
		return nil
	}
	return d.startPunts()
}

// Detach closes this handle without touching the data plane, which keeps running.
func (d *DP) Detach() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rd != nil {
		d.rd.Close()
		d.rd = nil
	}
	if d.erd != nil {
		d.erd.Close()
		d.erd = nil
	}
	d.closeHandles()
	d.isSet = false
	d.fail(errors.New("ebpfdp: detached"))
}

func (d *DP) closeHandles() {
	for _, p := range d.progs {
		p.Close()
	}
	for _, m := range d.maps {
		m.Close()
	}
	d.maps, d.progs = nil, nil
}

// runSyscall runs a SYSCALL program on ctx and returns its result and the context as the program
// left it.
func (d *DP) runSyscall(name string, ctx []byte) (uint32, []byte, error) {
	out := make([]byte, len(ctx))
	ret, err := d.progs[name].Run(&ebpf.RunOptions{Context: ctx, ContextOut: out})
	return ret, out, err
}

func (d *DP) startPunts() error {
	rd, err := ringbuf.NewReader(d.maps["punts"])
	if err != nil {
		return fmt.Errorf("ebpfdp: ring buffer: %w", err)
	}
	erd, err := ringbuf.NewReader(d.maps["mn_events"])
	if err != nil {
		rd.Close()
		return fmt.Errorf("ebpfdp: event ring buffer: %w", err)
	}
	d.rd, d.erd = rd, erd
	go d.readPunts(rd)
	go d.readEvents(erd)
	return nil
}

func (d *DP) readEvents(rd *ringbuf.Reader) {
	l := d.lay.bfdEvent
	for {
		rec, err := rd.Read()
		if err != nil {
			if !errors.Is(err, ringbuf.ErrClosed) {
				d.fail(fmt.Errorf("ebpfdp: reading liveness events: %w", err))
			}
			return
		}
		b := rec.RawSample
		if len(b) < l.size {
			continue
		}
		if d.cfg.OnLiveness != nil {
			d.cfg.OnLiveness(dpproto.LivenessEvent{
				Link:  l.getU32(b, "link"),
				State: uint8(l.getU32(b, "state")),
				Diag:  uint8(l.getU32(b, "diag")),
				Seq:   l.getU32(b, "seq"),
			})
		}
	}
}

// LivenessForce sets a link's liveness state from a local decision (path-vector timeout, admin down).
func (d *DP) LivenessForce(m dpproto.LivenessForce) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	l := d.lay.bfdArgs
	ctx := l.new()
	l.setU32(ctx, "slot", m.Link)
	l.setU32(ctx, "state", uint32(m.State))
	l.setU32(ctx, "diag", uint32(m.Diag))
	ret, _, err := d.runSyscall("mn_bfd_force", ctx)
	if err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "liveness of link %d: %v", m.Link, err)
	}
	if ret != 0 {
		return dpproto.Errorf(dpproto.CodeNotFound, "no liveness for link %d", m.Link)
	}
	return nil
}

func (d *DP) readPunts(rd *ringbuf.Reader) {
	l := d.lay.punt
	for {
		rec, err := rd.Read()
		if err != nil {
			if !errors.Is(err, ringbuf.ErrClosed) {
				d.fail(fmt.Errorf("ebpfdp: reading punts: %w", err))
			}
			return
		}
		b := rec.RawSample
		if len(b) < l.size {
			continue
		}
		n := int(l.getU32(b, "len"))
		if n > len(b)-l.size {
			continue
		}
		p := dpproto.Punt{
			Ingress: l.getU32(b, "link"),
			Proto:   l.getU8(b, "proto"),
			Src:     l.getU8(b, "src"),
			Dst:     l.getU8(b, "dst"),
			TTL:     l.getU8(b, "ttl"),
			Payload: append([]byte(nil), b[l.size:l.size+n]...),
		}
		if d.cfg.OnPunt != nil {
			d.cfg.OnPunt(p)
		}
	}
}

// Close tears the data plane down: device, sockets, timers, pins.
func (d *DP) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.teardownLocked()
	d.fail(errors.New("ebpfdp: closed"))
}

func (d *DP) teardownLocked() {
	if d.rd != nil {
		d.rd.Close()
		d.rd = nil
	}
	if d.erd != nil {
		d.erd.Close()
		d.erd = nil
	}
	if d.progs != nil {
		if _, _, err := d.runSyscall("wg_teardown", make([]byte, 8)); err != nil {
			d.cfg.Logf("ebpfdp: teardown: %v", err)
		}
	}
	d.closeHandles()
	if d.owner != nil {
		d.owner.Close()
		d.owner = nil
	}
	d.unpinAll()
	d.isSet = false
}

func (d *DP) ready() error {
	if !d.isSet {
		return dpproto.Errorf(dpproto.CodeNotReady, "device not configured")
	}
	return nil
}

var errRange = func(what string, id uint32) error {
	return dpproto.Errorf(dpproto.CodeInvalid, "%s %d out of range", what, id)
}

// ---- endpoints: struct bpfx_addr, port in network order ----

func (d *DP) putAddr(b []byte, s string) error {
	l := d.lay.addr
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return dpproto.Errorf(dpproto.CodeInvalid, "invalid endpoint %q (must be an ip:port literal): %v", s, err)
	}
	if ap.Addr().Zone() != "" {
		return dpproto.Errorf(dpproto.CodeInvalid, "endpoint %q: zones are not supported", s)
	}
	a := ap.Addr().Unmap()
	if a.Is4() {
		l.setU16(b, "family", afINET)
	} else {
		l.setU16(b, "family", afINET6)
	}
	binary.BigEndian.PutUint16(l.bytes(b, "port"), ap.Port())
	copy(l.bytes(b, "addr"), a.AsSlice())
	return nil
}

func (d *DP) getAddr(b []byte) string {
	l := d.lay.addr
	port := binary.BigEndian.Uint16(l.bytes(b, "port"))
	addr := l.bytes(b, "addr")
	switch l.getU16(b, "family") {
	case afINET:
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(addr[:4])), port).String()
	case afINET6:
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(addr)), port).String()
	}
	return ""
}

func (d *DP) peerAddrOff() int {
	return d.lay.peer.f("ep").off + d.lay.ep.f("addr").off
}

// ---- links ----

func (d *DP) peerRaw(slot uint32) ([]byte, error) {
	raw := d.lay.peer.new()
	if err := d.maps["wg_peers"].Lookup(slot, raw); err != nil {
		return nil, fmt.Errorf("ebpfdp: reading peer %d: %w", slot, err)
	}
	return raw, nil
}

func (d *DP) LinkSet(m dpproto.LinkSet) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	if m.PeerID == 0 || m.PeerID >= maxPeers {
		return errRange("peer id", m.PeerID)
	}
	if m.PeerID == d.devCfg.LocalID {
		return dpproto.Errorf(dpproto.CodeInvalid, "peer id %d is this node", m.PeerID)
	}
	if m.PubKey == ([32]byte{}) {
		return dpproto.Errorf(dpproto.CodeInvalid, "public key is zero")
	}
	pl := d.lay.peer
	raw, err := d.peerRaw(m.PeerID)
	if err != nil {
		return err
	}
	if pl.getU32(raw, "present") != 0 {
		if !bytes.Equal(pl.bytes(raw, "pub"), m.PubKey[:]) {
			return dpproto.Errorf(dpproto.CodeExists, "link %d exists with another public key", m.PeerID)
		}
		if m.Endpoint == "" {
			return nil
		}
		ul := d.lay.peerUpd
		ctx := ul.new()
		ul.setU32(ctx, "slot", m.PeerID)
		ul.setU32(ctx, "flags", updEndpoint)
		if err := d.putAddr(ctx[ul.f("ep").off:], m.Endpoint); err != nil {
			return err
		}
		ret, _, err := d.runSyscall("wg_peer_update", ctx)
		if err != nil || ret != 0 {
			return dpproto.Errorf(dpproto.CodeInternal, "setting endpoint of link %d: ret %d err %v", m.PeerID, int32(ret), err)
		}
		return nil
	}
	var existing uint32
	if err := d.maps["wg_peer_by_pub"].Lookup(m.PubKey, &existing); err == nil && existing != m.PeerID {
		return dpproto.Errorf(dpproto.CodeExists, "public key already used by link %d", existing)
	}
	fresh := pl.new()
	copy(pl.bytes(fresh, "pub"), m.PubKey[:])
	if m.Endpoint != "" {
		if err := d.putAddr(fresh[d.peerAddrOff():], m.Endpoint); err != nil {
			return err
		}
	}
	if err := d.maps["wg_peers"].Update(m.PeerID, fresh, ebpf.UpdateAny); err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "writing link %d: %v", m.PeerID, err)
	}
	ctx := d.lay.peerArgs.new()
	d.lay.peerArgs.setU32(ctx, "slot", m.PeerID)
	ret, out, err := d.runSyscall("wg_peer_prepare", ctx)
	if err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "committing link %d: %v", m.PeerID, err)
	}
	if ret != 0 {
		errno := int32(d.lay.peerArgs.getU32(out, "err"))
		_ = d.maps["wg_peers"].Update(m.PeerID, pl.new(), ebpf.UpdateAny)
		return dpproto.Errorf(codeFor(errno), "committing link %d failed (errno %d)", m.PeerID, -errno)
	}
	return nil
}

func codeFor(errno int32) uint16 {
	switch -errno {
	case int32(unix.EEXIST):
		return dpproto.CodeExists
	case int32(unix.ENOENT), int32(unix.ENODEV):
		return dpproto.CodeNotFound
	case int32(unix.EINVAL):
		return dpproto.CodeInvalid
	case int32(unix.EADDRINUSE):
		return dpproto.CodeAddrInUse
	}
	return dpproto.CodeInternal
}

func (d *DP) LinkDel(peerID uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	if peerID >= maxPeers {
		return errRange("peer id", peerID)
	}
	raw, err := d.peerRaw(peerID)
	if err != nil {
		return err
	}
	if d.lay.peer.getU32(raw, "present") == 0 {
		return dpproto.Errorf(dpproto.CodeNotFound, "no link %d", peerID)
	}
	ctx := d.lay.peerArgs.new()
	d.lay.peerArgs.setU32(ctx, "slot", peerID)
	ret, _, err := d.runSyscall("wg_peer_remove", ctx)
	if err != nil || ret != 0 {
		return dpproto.Errorf(dpproto.CodeInternal, "removing link %d: ret %d err %v", peerID, int32(ret), err)
	}
	return nil
}

func (d *DP) LinkList() ([]dpproto.LinkInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return nil, err
	}
	pl := d.lay.peer
	var out []dpproto.LinkInfo
	for slot := uint32(0); slot < maxPeers; slot++ {
		raw, err := d.peerRaw(slot)
		if err != nil {
			return nil, err
		}
		if pl.getU32(raw, "present") == 0 {
			continue
		}
		li := dpproto.LinkInfo{
			PeerID:                slot,
			Endpoint:              d.getAddr(raw[d.peerAddrOff():]),
			LastHandshakeUnixNano: int64(pl.getU64(raw, "walltime_last_handshake")),
			RxBytes:               pl.getU64(raw, "rx_bytes"),
			TxBytes:               pl.getU64(raw, "tx_bytes"),
		}
		copy(li.PubKey[:], pl.bytes(raw, "pub"))
		bl := d.lay.bfd
		braw := bl.new()
		if err := d.maps["mn_bfd"].LookupWithFlags(slot, braw, ebpf.LookupLock); err == nil {
			li.LivenessValid = true
			li.LivenessSeq = bl.getU32(braw, "seq")
			li.LivenessState = uint8(bl.getU32(braw, "state"))
			li.LivenessDiag = uint8(bl.getU32(braw, "diag"))
			li.Passive = bl.getU32(braw, "passive") != 0
			li.LocalDiscriminator = bl.getU32(braw, "local_disc")
			li.RemoteDiscriminator = bl.getU32(braw, "remote_disc")
			li.RemoteDesiredMinTXUs = bl.getU32(braw, "remote_tx_us")
			li.RemoteRequiredMinRXUs = bl.getU32(braw, "remote_rx_us")
		}
		out = append(out, li)
	}
	return out, nil
}

// ---- the device ----

// Device is the data plane's network device. It exists as long as the data plane does; if something
// removed it behind our back the error says so.
func (d *DP) Device() (dpproto.DeviceInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return dpproto.DeviceInfo{}, err
	}
	name := d.devCfg.IfName
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return dpproto.DeviceInfo{}, dpproto.Errorf(dpproto.CodeNotFound, "device %q is gone: %v", name, err)
	}
	return dpproto.DeviceInfo{Name: ifc.Name, IfIndex: uint32(ifc.Index)}, nil
}

// ---- routes ----

func (d *DP) setRoute(dst, nh uint32, present bool) error {
	l := d.lay.route
	ctx := l.new()
	l.setU32(ctx, "dst", dst)
	l.setU32(ctx, "nexthop", nh)
	if present {
		l.setU32(ctx, "present", 1)
	}
	ret, out, err := d.runSyscall("mn_route_set", ctx)
	if err != nil {
		return fmt.Errorf("ebpfdp: route %d: %w", dst, err)
	}
	if ret != 0 {
		errno := int32(l.getU32(out, "err"))
		return dpproto.Errorf(codeFor(errno), "route %d via %d failed (errno %d)", dst, nh, -errno)
	}
	return nil
}

func (d *DP) RouteSet(routes []dpproto.Route) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	want := make(map[uint32]uint32, len(routes))
	for _, r := range routes {
		if r.Dst == 0 || r.Dst >= maxPeers || r.NextHop >= maxPeers {
			return dpproto.Errorf(dpproto.CodeInvalid, "route %d via %d out of range", r.Dst, r.NextHop)
		}
		if _, dup := want[r.Dst]; dup {
			return dpproto.Errorf(dpproto.CodeInvalid, "duplicate route to %d", r.Dst)
		}
		want[r.Dst] = r.NextHop
	}
	for dst := uint32(1); dst < maxPeers; dst++ {
		var cur [2]uint32
		if err := d.maps["routes"].Lookup(dst, &cur); err != nil {
			return fmt.Errorf("ebpfdp: reading route %d: %w", dst, err)
		}
		nh, ok := want[dst]
		if cur[0] != 0 && !ok {
			if err := d.setRoute(dst, 0, false); err != nil {
				return err
			}
		}
		if ok && (cur[0] == 0 || cur[1] != nh) {
			if err := d.setRoute(dst, nh, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *DP) RouteList() ([]dpproto.Route, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return nil, err
	}
	var out []dpproto.Route
	for dst := uint32(0); dst < maxPeers; dst++ {
		var cur [2]uint32
		if err := d.maps["routes"].Lookup(dst, &cur); err != nil {
			return nil, fmt.Errorf("ebpfdp: reading route %d: %w", dst, err)
		}
		if cur[0] != 0 {
			out = append(out, dpproto.Route{Dst: dst, NextHop: cur[1]})
		}
	}
	return out, nil
}

// ---- inject, stats ----

func (d *DP) Inject(m dpproto.Inject) error {
	if len(m.Payload) > injectMax {
		return dpproto.Errorf(dpproto.CodeInvalid, "payload of %d bytes is too long", len(m.Payload))
	}
	l := d.lay.inject
	ctx := l.new()
	l.setU32(ctx, "link", m.Link)
	l.setU32(ctx, "len", uint32(len(m.Payload)))
	l.setU8(ctx, "proto", m.Proto)
	l.setU8(ctx, "dst", m.Dst)
	l.setU8(ctx, "ttl", m.TTL)
	copy(ctx[l.f("data").off:], m.Payload)
	d.mu.Lock()
	if err := d.ready(); err != nil {
		d.mu.Unlock()
		return err
	}
	ret, out, err := d.runSyscall("mn_inject", ctx)
	d.mu.Unlock()
	if err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "inject: %v", err)
	}
	if ret != 0 {
		return dpproto.Errorf(codeFor(int32(l.getU32(out, "err"))), "inject to link %d dropped", m.Link)
	}
	return nil
}

func (d *DP) Stats() ([]dpproto.Stat, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return nil, err
	}
	out := make([]dpproto.Stat, 0, len(d.statNames))
	for i := range d.statNames {
		var per []uint64
		if err := d.maps["stats"].Lookup(uint32(i), &per); err != nil {
			return nil, fmt.Errorf("ebpfdp: reading counter %d: %w", i, err)
		}
		var sum uint64
		for _, v := range per {
			sum += v
		}
		out = append(out, dpproto.Stat{ID: uint16(i + 1), Value: sum})
	}
	return out, nil
}
