// Package ebpfdp is the data plane: it loads the melnode eBPF object, sets up the devices and UDP
// sockets through its control programs, and implements dpproto.Datapath on top of it.
//
// The object is the data plane; this package only configures it and carries the control plane's
// packets in and out. Everything the helper knows about the object's maps and structs is read
// from its BTF at start-up (see layout.go).
//
// The state lives in the kernel and is pinned under PinDir (the maps keep the protocol timers
// alive). If the helper dies, the next DeviceSet with the same configuration picks the state up
// again; a clean Close tears everything down.
package ebpfdp

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"melnode/dpproto"
)

const (
	DefaultPinDir = "/sys/fs/bpf/melnode"

	maxPeers   = 256
	afINET     = 2
	afINET6    = 10
	injectMax  = 4096
	timeScale1 = 1
)

// Config says where the object is and how it is run.
type Config struct {
	ObjectPath string // melnode.bpf.o
	PinDir     string // default DefaultPinDir
	HubName    string // the device that owns the UDP sockets
	// TimeScale divides the protocol timers (tests only); 0 or 1 is real time.
	TimeScale uint64
	// UnderLoadThreshold is how many handshake messages a second make the node demand cookies;
	// 0 keeps the object's default. Tests only.
	UnderLoadThreshold uint32
	// Variables sets named `const volatile` variables of the object before it is loaded: the way
	// to tune a policy compiled into the data plane (thresholds, lane counts, ...) without this
	// helper knowing about it. Unknown names are an error.
	Variables map[string]uint64
	// OnPunt receives control-plane packets, from its own goroutine; it must not block.
	OnPunt func(dpproto.Punt)
	Logf   func(format string, args ...any)
}

type layouts struct {
	dev, peer, ep, setup, peerCtl, tun, inject, epArgs, punt *layout
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
	coll   *ebpf.Collection // nil when attached to pinned state
	rd     *ringbuf.Reader

	injMu sync.Mutex // one inject at a time: mn_inject shares a scratch buffer

	done    chan struct{}
	errOnce sync.Once
	err     error
}

var _ dpproto.Datapath = (*DP)(nil)

// the timers the object lets us scale, with their unscaled values in seconds.
var timers = map[string]uint64{
	"t_reject_after":      180,
	"t_rekey_after":       120,
	"t_rekey_timeout":     5,
	"t_keepalive_timeout": 10,
	"t_rekey_attempt":     90,
	"t_cookie_secret":     120,
	"t_cookie_latency":    5,
}

var mapNames = []string{
	"dev_map", "peers", "kps", "idx_map", "peer_by_pub", "routes", "tun_by_peer",
	"peer_by_ifindex", "punts", "punt_scratch", "hs_scratch", "stats", "peer_stats",
	"ratelimit", "tick_map", "inj_scratch",
}

var progNames = []string{
	"mn_xmit", "mn_rx", "mn_kick", "mn_setup", "mn_teardown", "mn_inject", "mn_tun_add",
	"mn_tun_del", "mn_peer_commit", "mn_peer_remove", "mn_peer_set_ep",
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
	if cfg.HubName == "" {
		cfg.HubName = "node-hub"
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
	for n := range timers {
		if d.spec.Variables[n] == nil {
			return fmt.Errorf("no variable %q", n)
		}
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
	d.lay.ep = mk("bpfx_endpoint", map[string]int{"family": 2, "port": 2, "src_ifindex": 4, "addr": 16, "src": 16, "tos": 1})
	d.lay.dev = mk("mn_dev", map[string]int{"hub_ifindex": 4, "port": 2, "local_id": 2, "mtu": 4, "fwmark": 4, "spriv": 32, "spub": 32})
	d.lay.peer = mk("mn_peer", map[string]int{"present": 4, "pub": 32, "ep": 0, "last_hs_boot_ns": 8})
	d.lay.setup = mk("mn_setup_args", map[string]int{"hub_name": 16, "xmit_fd": 4, "rx_fd": 4, "mtu": 4, "port": 2, "local_id": 2, "fwmark": 4, "hub_ifindex": 4, "stage": 4, "err": 4})
	d.lay.peerCtl = mk("mn_peer_ctl", map[string]int{"slot": 4, "err": 4})
	d.lay.tun = mk("mn_tun_args", map[string]int{"name": 16, "peer": 4, "xmit_fd": 4, "ifindex": 4, "err": 4})
	d.lay.inject = mk("mn_inject_args", map[string]int{"link": 4, "len": 4, "proto": 1, "dst": 1, "ttl": 1, "err": 4})
	d.lay.epArgs = mk("mn_ep_args", map[string]int{"slot": 4, "err": 4, "ep": 0})
	d.lay.punt = mk("mn_punt_hdr", map[string]int{"link": 4, "len": 4, "proto": 1, "src": 1, "dst": 1, "ttl": 1})
	if err != nil {
		return err
	}
	// The counters' names come from the object when it says them (its enum is anchored in BTF), so
	// an object with more counters than this helper knows still reports all of them.
	d.statNames = statNames
	var enum *btf.Enum
	if types.TypeByName("mn_stat_id", &enum) == nil {
		var names []string
		for _, v := range enum.Values {
			if n := strings.TrimPrefix(v.Name, "MN_ST_"); n != "MAX" {
				names = append(names, n)
			}
		}
		if len(names) >= len(statNames) {
			d.statNames = names
			registerStatNames(names)
		}
	}
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

func (d *DP) hubAlive(ifindex uint32) bool {
	if ifindex == 0 {
		return false
	}
	_, err := net.InterfaceByIndex(int(ifindex))
	return err == nil
}

// DeviceSet creates the data plane's devices and sockets, or finds them already there. Called
// again with the same configuration it returns the public key; with another it fails.
func (d *DP) DeviceSet(m dpproto.DeviceSet) (dpproto.DeviceSetReply, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m.LocalID > 255 {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeInvalid, "local id %d out of range", m.LocalID)
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
	if d.isSet {
		return d.sameConfig(m)
	}
	if d.pinnedExists() {
		ok, err := d.attach(m)
		if err != nil {
			return dpproto.DeviceSetReply{}, err
		}
		if ok {
			return dpproto.DeviceSetReply{PubKey: d.pub}, nil
		}
		d.cfg.Logf("ebpfdp: stale state in %s (no device any more), starting afresh", d.cfg.PinDir)
		d.unpinAll()
	}
	if err := d.create(m); err != nil {
		return dpproto.DeviceSetReply{}, err
	}
	return dpproto.DeviceSetReply{PubKey: d.pub}, nil
}

func (d *DP) sameConfig(m dpproto.DeviceSet) (dpproto.DeviceSetReply, error) {
	if m != d.devCfg {
		return dpproto.DeviceSetReply{}, dpproto.Errorf(dpproto.CodeExists, "already configured differently")
	}
	return dpproto.DeviceSetReply{PubKey: d.pub}, nil
}

func (d *DP) pinPath(name string) string { return filepath.Join(d.cfg.PinDir, name) }

func (d *DP) pinnedExists() bool {
	_, err := os.Stat(d.pinPath("dev_map"))
	return err == nil
}

func (d *DP) unpinAll() {
	// bpffs entries are removed with unlink; the directory with rmdir.
	for _, n := range mapNames {
		_ = os.Remove(d.pinPath(n))
	}
	for _, n := range progNames {
		_ = os.Remove(d.pinPath("prog_" + n))
	}
	_ = os.Remove(d.cfg.PinDir)
}

func (d *DP) verifierErr(what string, err error) error {
	var ve *ebpf.VerifierError
	if errors.As(err, &ve) {
		return fmt.Errorf("%s: %+v", what, ve)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func (d *DP) create(m dpproto.DeviceSet) error {
	if err := os.MkdirAll(d.cfg.PinDir, 0o755); err != nil {
		return fmt.Errorf("ebpfdp: %w", err)
	}
	spec := d.spec.Copy()
	for name, ms := range spec.Maps {
		if !strings.HasPrefix(name, ".") {
			ms.Pinning = ebpf.PinByName
		}
	}
	for n, secs := range timers {
		v := secs * uint64(time.Second) / d.cfg.TimeScale
		if err := spec.Variables[n].Set(v); err != nil {
			return fmt.Errorf("ebpfdp: setting %s: %w", n, err)
		}
	}
	for name, v := range d.cfg.Variables {
		if err := setVariable(spec, name, v); err != nil {
			return fmt.Errorf("ebpfdp: %w", err)
		}
	}
	if d.cfg.UnderLoadThreshold != 0 {
		if err := spec.Variables["under_load_threshold"].Set(d.cfg.UnderLoadThreshold); err != nil {
			return fmt.Errorf("ebpfdp: setting the load threshold: %w", err)
		}
	}
	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{PinPath: d.cfg.PinDir},
	})
	if err != nil {
		d.unpinAll()
		return d.verifierErr("ebpfdp: loading the object", err)
	}
	d.coll = coll
	d.maps, d.progs = coll.Maps, coll.Programs
	cleanup := func(err error) error {
		d.teardownLocked()
		return err
	}

	dev := d.lay.dev.new()
	copy(d.lay.dev.bytes(dev, "spriv"), m.PrivateKey[:])
	if err := d.maps["dev_map"].Update(uint32(0), dev, ebpf.UpdateAny); err != nil {
		return cleanup(fmt.Errorf("ebpfdp: writing the device: %w", err))
	}

	ctx := d.lay.setup.new()
	// Optional programs a policy may use (the lanes of bpfx, say): handed to mn_setup if the object
	// has them and its setup args have the field, so adding them is an object-only change.
	for _, opt := range []struct{ prog, field string }{{"mn_lane_tx", "lane_tx_fd"}, {"mn_lane_rx", "lane_rx_fd"}} {
		if _, ok := d.lay.setup.fields[opt.field]; !ok {
			continue
		}
		fd := int32(-1)
		if p := d.progs[opt.prog]; p != nil {
			fd = int32(p.FD())
		}
		d.lay.setup.setU32(ctx, opt.field, uint32(fd))
	}
	copy(d.lay.setup.bytes(ctx, "hub_name"), d.cfg.HubName)
	d.lay.setup.setU32(ctx, "xmit_fd", uint32(d.progs["mn_xmit"].FD()))
	d.lay.setup.setU32(ctx, "rx_fd", uint32(d.progs["mn_rx"].FD()))
	d.lay.setup.setU32(ctx, "mtu", m.MTU)
	d.lay.setup.setU16(ctx, "port", m.ListenPort)
	d.lay.setup.setU16(ctx, "local_id", uint16(m.LocalID))
	d.lay.setup.setU32(ctx, "fwmark", m.Fwmark)
	ret, out, err := d.runSyscall("mn_setup", ctx)
	if err != nil {
		return cleanup(fmt.Errorf("ebpfdp: running mn_setup: %w", err))
	}
	if ret != 0 {
		stage := int32(d.lay.setup.getU32(out, "stage"))
		errno := int32(d.lay.setup.getU32(out, "err"))
		return cleanup(setupError(stage, errno, d.cfg.HubName, m.ListenPort))
	}

	if err := d.maps["dev_map"].Lookup(uint32(0), dev); err != nil {
		return cleanup(fmt.Errorf("ebpfdp: reading the device: %w", err))
	}
	copy(d.pub[:], d.lay.dev.bytes(dev, "spub"))

	ret, err = d.progs["mn_kick"].Run(&ebpf.RunOptions{Data: make([]byte, 14)})
	if err != nil || ret != 0 {
		return cleanup(fmt.Errorf("ebpfdp: starting the timer: ret %d: %v", ret, err))
	}
	for _, n := range progNames {
		if err := d.progs[n].Pin(d.pinPath("prog_" + n)); err != nil {
			return cleanup(fmt.Errorf("ebpfdp: pinning %s: %w", n, err))
		}
	}
	if err := d.startPunts(); err != nil {
		return cleanup(err)
	}
	d.isSet, d.devCfg = true, m
	return nil
}

func setupError(stage, errno int32, hub string, port uint16) error {
	what := map[int32]string{
		1: "deriving the device keys", 2: "creating device " + hub,
		3: fmt.Sprintf("opening UDP port %d", port), 4: "opening the IPv6 socket",
	}[stage]
	if what == "" {
		what = fmt.Sprintf("stage %d", stage)
	}
	code := dpproto.CodeInternal
	switch -errno {
	case int32(unix.EADDRINUSE):
		code = dpproto.CodeAddrInUse
	case int32(unix.EEXIST):
		code = dpproto.CodeExists
	case int32(unix.EINVAL):
		code = dpproto.CodeInvalid
	}
	return dpproto.Errorf(code, "setup failed %s: %v", what, unix.Errno(-errno))
}

// loadPinned opens the pinned maps and programs and reads the node's configuration from them.
// ok is false if there is no usable state (nothing pinned, or its hub device is gone).
func (d *DP) loadPinned() (cfg dpproto.DeviceSet, pub [32]byte, ok bool) {
	maps := make(map[string]*ebpf.Map, len(mapNames))
	progs := make(map[string]*ebpf.Program, len(progNames))
	closeAll := func() {
		for _, mp := range maps {
			mp.Close()
		}
		for _, p := range progs {
			p.Close()
		}
	}
	for _, n := range mapNames {
		mp, err := ebpf.LoadPinnedMap(d.pinPath(n), nil)
		if err != nil {
			closeAll()
			return cfg, pub, false
		}
		maps[n] = mp
	}
	for _, n := range progNames {
		p, err := ebpf.LoadPinnedProgram(d.pinPath("prog_"+n), nil)
		if err != nil {
			closeAll()
			return cfg, pub, false
		}
		progs[n] = p
	}
	dev := d.lay.dev.new()
	if err := maps["dev_map"].Lookup(uint32(0), dev); err != nil || !d.hubAlive(d.lay.dev.getU32(dev, "hub_ifindex")) {
		closeAll()
		return cfg, pub, false
	}
	cfg = dpproto.DeviceSet{
		LocalID:    uint32(d.lay.dev.getU16(dev, "local_id")),
		ListenPort: d.lay.dev.getU16(dev, "port"),
		Fwmark:     d.lay.dev.getU32(dev, "fwmark"),
		MTU:        d.lay.dev.getU32(dev, "mtu"),
	}
	copy(cfg.PrivateKey[:], d.lay.dev.bytes(dev, "spriv"))
	copy(pub[:], d.lay.dev.bytes(dev, "spub"))
	d.maps, d.progs = maps, progs
	return cfg, pub, true
}

// attach picks up pinned state left by an earlier helper. It reports false if the state is stale
// (its hub device is gone).
func (d *DP) attach(m dpproto.DeviceSet) (bool, error) {
	have, pub, ok := d.loadPinned()
	if !ok {
		return false, nil
	}
	if have != m {
		d.closeHandles()
		return false, dpproto.Errorf(dpproto.CodeExists, "data plane state in %s was made with another configuration; remove it or fix the configuration", d.cfg.PinDir)
	}
	d.pub = pub
	if err := d.startPunts(); err != nil {
		d.closeHandles()
		return false, err
	}
	d.isSet, d.devCfg = true, m
	d.cfg.Logf("ebpfdp: attached to existing state in %s", d.cfg.PinDir)
	return true, nil
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
	d.closeHandles()
	d.isSet = false
	d.fail(errors.New("ebpfdp: detached"))
}

// closeHandles drops this process's references to the maps and programs.
func (d *DP) closeHandles() {
	for _, p := range d.progs {
		p.Close()
	}
	for _, m := range d.maps {
		m.Close()
	}
	if d.coll != nil {
		d.coll.Close()
		d.coll = nil
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
	d.rd = rd
	go d.readPunts(rd)
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

// Close tears the data plane down: devices, sockets, timers, pins.
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
	if d.progs != nil {
		if p := d.progs["mn_teardown"]; p != nil {
			ctx := make([]byte, 8)
			if _, _, err := d.runSyscall("mn_teardown", ctx); err != nil {
				d.cfg.Logf("ebpfdp: teardown: %v", err)
			}
		}
	}
	d.unpinAll()
	d.closeHandles()
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

// ---- endpoints ----

func (d *DP) putEndpoint(b []byte, s string) error {
	l := d.lay.ep
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
	l.setU16(b, "port", ap.Port())
	copy(l.bytes(b, "addr"), a.AsSlice())
	return nil
}

func (d *DP) getEndpoint(b []byte) string {
	l := d.lay.ep
	port := l.getU16(b, "port")
	addr := l.bytes(b, "addr")
	switch l.getU16(b, "family") {
	case afINET:
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(addr[:4])), port).String()
	case afINET6:
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(addr)), port).String()
	}
	return ""
}

// ---- links ----

func (d *DP) peerRaw(slot uint32) ([]byte, error) {
	raw := d.lay.peer.new()
	if err := d.maps["peers"].Lookup(slot, raw); err != nil {
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
	if m.PeerID >= maxPeers {
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
	epOff := pl.f("ep").off

	if pl.getU32(raw, "present") != 0 {
		if !bytes.Equal(pl.bytes(raw, "pub"), m.PubKey[:]) {
			return dpproto.Errorf(dpproto.CodeExists, "link %d exists with another public key", m.PeerID)
		}
		if m.Endpoint == "" {
			return nil
		}
		ctx := d.lay.epArgs.new()
		d.lay.epArgs.setU32(ctx, "slot", m.PeerID)
		if err := d.putEndpoint(ctx[d.lay.epArgs.f("ep").off:], m.Endpoint); err != nil {
			return err
		}
		ret, out, err := d.runSyscall("mn_peer_set_ep", ctx)
		if err != nil || ret != 0 {
			return dpproto.Errorf(dpproto.CodeInternal, "setting endpoint of link %d: ret %d err %v (%d)", m.PeerID, ret, err, int32(d.lay.epArgs.getU32(out, "err")))
		}
		return nil
	}

	// A public key can name only one link.
	var existing uint32
	if err := d.maps["peer_by_pub"].Lookup(m.PubKey, &existing); err == nil && existing != m.PeerID {
		return dpproto.Errorf(dpproto.CodeExists, "public key already used by link %d", existing)
	}

	fresh := pl.new()
	copy(pl.bytes(fresh, "pub"), m.PubKey[:])
	if m.Endpoint != "" {
		if err := d.putEndpoint(fresh[epOff:], m.Endpoint); err != nil {
			return err
		}
	}
	if err := d.maps["peers"].Update(m.PeerID, fresh, ebpf.UpdateAny); err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "writing link %d: %v", m.PeerID, err)
	}
	ctx := d.lay.peerCtl.new()
	d.lay.peerCtl.setU32(ctx, "slot", m.PeerID)
	ret, out, err := d.runSyscall("mn_peer_commit", ctx)
	if err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "committing link %d: %v", m.PeerID, err)
	}
	if ret != 0 {
		return dpproto.Errorf(codeFor(int32(d.lay.peerCtl.getU32(out, "err"))), "committing link %d failed (errno %d)", m.PeerID, -int32(d.lay.peerCtl.getU32(out, "err")))
	}
	return nil
}

func codeFor(errno int32) uint16 {
	switch -errno {
	case int32(unix.EEXIST):
		return dpproto.CodeExists
	case int32(unix.ENOENT):
		return dpproto.CodeNotFound
	case int32(unix.EINVAL):
		return dpproto.CodeInvalid
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
	ctx := d.lay.peerCtl.new()
	d.lay.peerCtl.setU32(ctx, "slot", peerID)
	ret, out, err := d.runSyscall("mn_peer_remove", ctx)
	if err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "removing link %d: %v", peerID, err)
	}
	if ret != 0 {
		return dpproto.Errorf(codeFor(int32(d.lay.peerCtl.getU32(out, "err"))), "no link %d", peerID)
	}
	return nil
}

func bootToUnix() (nowBoot uint64, off int64) {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts)
	nowBoot = uint64(ts.Nano())
	return nowBoot, time.Now().UnixNano() - int64(nowBoot)
}

func (d *DP) LinkList() ([]dpproto.LinkInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return nil, err
	}
	pl := d.lay.peer
	_, off := bootToUnix()
	var out []dpproto.LinkInfo
	for slot := uint32(0); slot < maxPeers; slot++ {
		raw, err := d.peerRaw(slot)
		if err != nil {
			return nil, err
		}
		if pl.getU32(raw, "present") == 0 {
			continue
		}
		li := dpproto.LinkInfo{PeerID: slot, Endpoint: d.getEndpoint(raw[pl.f("ep").off:])}
		copy(li.PubKey[:], pl.bytes(raw, "pub"))
		if hs := pl.getU64(raw, "last_hs_boot_ns"); hs != 0 {
			li.LastHandshakeUnixNano = int64(hs) + off
		}
		var per []struct{ Rx, Tx, RxP, TxP uint64 }
		if err := d.maps["peer_stats"].Lookup(slot, &per); err == nil {
			for _, c := range per {
				li.RxBytes += c.Rx
				li.TxBytes += c.Tx
			}
		}
		out = append(out, li)
	}
	return out, nil
}

// ---- per-peer devices ----

func (d *DP) tunIfindex(peerID uint32) (uint32, error) {
	var ifx uint32
	if err := d.maps["tun_by_peer"].Lookup(peerID, &ifx); err != nil {
		return 0, fmt.Errorf("ebpfdp: reading device of %d: %w", peerID, err)
	}
	return ifx, nil
}

func (d *DP) TunSet(m dpproto.TunSet) (dpproto.TunInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return dpproto.TunInfo{}, err
	}
	if m.PeerID >= maxPeers || m.PeerID == d.devCfg.LocalID {
		return dpproto.TunInfo{}, errRange("peer id", m.PeerID)
	}
	if m.Name == "" || len(m.Name) > 15 {
		return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeInvalid, "device name %q must be 1-15 characters", m.Name)
	}
	ifx, err := d.tunIfindex(m.PeerID)
	if err != nil {
		return dpproto.TunInfo{}, err
	}
	if ifx != 0 {
		ifc, err := net.InterfaceByIndex(int(ifx))
		if err == nil {
			if ifc.Name != m.Name {
				return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeExists, "device for %d is already %q", m.PeerID, ifc.Name)
			}
			return dpproto.TunInfo{PeerID: m.PeerID, Name: ifc.Name, IfIndex: ifx}, nil
		}
		// The device was removed behind our back: forget it and make a new one.
		d.forgetTun(m.PeerID)
	}
	l := d.lay.tun
	ctx := l.new()
	copy(l.bytes(ctx, "name"), m.Name)
	l.setU32(ctx, "peer", m.PeerID)
	l.setU32(ctx, "xmit_fd", uint32(d.progs["mn_xmit"].FD()))
	ret, out, err := d.runSyscall("mn_tun_add", ctx)
	if err != nil {
		return dpproto.TunInfo{}, dpproto.Errorf(dpproto.CodeInternal, "creating device %q: %v", m.Name, err)
	}
	if ret != 0 {
		e := int32(l.getU32(out, "err"))
		return dpproto.TunInfo{}, dpproto.Errorf(codeFor(e), "creating device %q: %v", m.Name, unix.Errno(-e))
	}
	return dpproto.TunInfo{PeerID: m.PeerID, Name: m.Name, IfIndex: l.getU32(out, "ifindex")}, nil
}

// forgetTun clears the data plane's record of a device that no longer exists.
func (d *DP) forgetTun(peerID uint32) {
	var ifx uint32
	if d.maps["tun_by_peer"].Lookup(peerID, &ifx) == nil && ifx != 0 {
		_ = d.maps["peer_by_ifindex"].Delete(ifx)
	}
	_ = d.maps["tun_by_peer"].Update(peerID, uint32(0), ebpf.UpdateAny)
}

func (d *DP) TunDel(peerID uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	if peerID >= maxPeers {
		return errRange("peer id", peerID)
	}
	l := d.lay.tun
	ctx := l.new()
	l.setU32(ctx, "peer", peerID)
	ret, out, err := d.runSyscall("mn_tun_del", ctx)
	if err != nil {
		return dpproto.Errorf(dpproto.CodeInternal, "deleting device of %d: %v", peerID, err)
	}
	if ret != 0 {
		return dpproto.Errorf(codeFor(int32(l.getU32(out, "err"))), "no device for %d", peerID)
	}
	return nil
}

func (d *DP) TunList() ([]dpproto.TunInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return nil, err
	}
	var out []dpproto.TunInfo
	for id := uint32(0); id < maxPeers; id++ {
		ifx, err := d.tunIfindex(id)
		if err != nil {
			return nil, err
		}
		if ifx == 0 {
			continue
		}
		ifc, err := net.InterfaceByIndex(int(ifx))
		if err != nil {
			continue
		}
		out = append(out, dpproto.TunInfo{PeerID: id, Name: ifc.Name, IfIndex: ifx})
	}
	return out, nil
}

// ---- routes ----

func (d *DP) RouteSet(routes []dpproto.Route) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ready(); err != nil {
		return err
	}
	want := make(map[uint32]uint32, len(routes))
	for _, r := range routes {
		if r.Dst >= maxPeers || r.NextHop >= maxPeers {
			return dpproto.Errorf(dpproto.CodeInvalid, "route %d via %d out of range", r.Dst, r.NextHop)
		}
		if _, dup := want[r.Dst]; dup {
			return dpproto.Errorf(dpproto.CodeInvalid, "duplicate route to %d", r.Dst)
		}
		want[r.Dst] = r.NextHop
	}
	// struct mn_route is {present, nexthop}.
	for dst := uint32(0); dst < maxPeers; dst++ {
		var cur [2]uint32
		if err := d.maps["routes"].Lookup(dst, &cur); err != nil {
			return fmt.Errorf("ebpfdp: reading route %d: %w", dst, err)
		}
		nh, ok := want[dst]
		next := [2]uint32{}
		if ok {
			next = [2]uint32{1, nh}
		}
		if cur == next {
			continue
		}
		if err := d.maps["routes"].Update(dst, next, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("ebpfdp: writing route %d: %w", dst, err)
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
	d.mu.Lock()
	err := d.ready()
	d.mu.Unlock()
	if err != nil {
		return err
	}
	l := d.lay.inject
	dataOff := l.f("data").off
	ctx := make([]byte, dataOff+len(m.Payload))
	l.setU32(ctx, "link", m.Link)
	l.setU32(ctx, "len", uint32(len(m.Payload)))
	l.setU8(ctx, "proto", m.Proto)
	l.setU8(ctx, "dst", m.Dst)
	l.setU8(ctx, "ttl", m.TTL)
	copy(ctx[dataOff:], m.Payload)
	d.injMu.Lock()
	ret, out, err := d.runSyscall("mn_inject", ctx)
	d.injMu.Unlock()
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

// setVariable sets a global of the object, whatever width it has.
func setVariable(spec *ebpf.CollectionSpec, name string, v uint64) error {
	vs := spec.Variables[name]
	if vs == nil {
		return fmt.Errorf("the object has no variable %q to set", name)
	}
	switch vs.Size() {
	case 1:
		return vs.Set(uint8(v))
	case 2:
		return vs.Set(uint16(v))
	case 4:
		return vs.Set(uint32(v))
	case 8:
		return vs.Set(v)
	}
	return fmt.Errorf("variable %q has an unsupported size %d", name, vs.Size())
}
