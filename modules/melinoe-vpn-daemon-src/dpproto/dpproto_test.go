package dpproto

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// roundTrip encodes a message's attributes and decodes them again, going
// through the real framing.
func roundTrip(t *testing.T, put func(*nlb), get func(attrs) error) {
	t.Helper()
	b := newNL(FamilyID, 0, 1, 0, CmdHello, uint8(Version))
	put(b)
	msgs, err := splitMessages(b.bytes())
	if err != nil || len(msgs) != 1 {
		t.Fatalf("split: %v %d", err, len(msgs))
	}
	a, err := msgs[0].attrs()
	if err != nil {
		t.Fatal(err)
	}
	if err := get(a); err != nil {
		t.Fatal(err)
	}
}

func TestCodecRoundTrip(t *testing.T) {
	var k [32]byte
	for i := range k {
		k[i] = byte(i)
	}
	{
		want := LinkAdd{PeerID: 7, PubKey: k, Endpoint: "[2001:db8::1]:60198"}
		var got LinkAdd
		roundTrip(t, func(b *nlb) {
			if err := want.put(b); err != nil {
				t.Fatal(err)
			}
		}, got.get)
		if got != want {
			t.Fatalf("LinkAdd: %+v", got)
		}
	}
	{
		want := LinkAdd{PeerID: 7, PubKey: k} // listen-only: no endpoint attribute at all
		var got LinkAdd
		roundTrip(t, func(b *nlb) { _ = want.put(b) }, got.get)
		if got != want {
			t.Fatalf("listen-only LinkAdd: %+v", got)
		}
	}
	check := func(name string, put func(*nlb), get func(attrs) error, eq func() bool) {
		t.Helper()
		roundTrip(t, put, get)
		if !eq() {
			t.Fatalf("%s did not round trip", name)
		}
	}
	hr, ghr := HelloReply{Version: Version, PID: 4242, Configured: true}, HelloReply{}
	check("HelloReply", hr.put, ghr.get, func() bool { return ghr == hr })
	ds, gds := DeviceSet{LocalID: 3, PrivateKey: k, ListenPort: 60198, Fwmark: 0x1234, MTU: 1416}, DeviceSet{}
	check("DeviceSet", ds.put, gds.get, func() bool { return gds == ds })
	dr, gdr := DeviceSetReply{PubKey: k}, DeviceSetReply{}
	check("DeviceSetReply", dr.put, gdr.get, func() bool { return gdr == dr })
	tc, gtc := TunCreate{PeerID: 4, Name: "node-4"}, TunCreate{}
	check("TunCreate", tc.put, gtc.get, func() bool { return gtc == tc })
	tr, gtr := TunCreateReply{Name: "node-4", Started: true}, TunCreateReply{}
	check("TunCreateReply", tr.put, gtr.get, func() bool { return gtr == tr })
	ti, gti := TunInfo{PeerID: 4, Name: "node-4", MTU: 1416, Started: true}, TunInfo{}
	check("TunInfo", ti.put, gti.get, func() bool { return gti == ti })
	rt, grt := Route{Dst: 3, NextHop: 4}, Route{}
	check("Route", rt.put, grt.get, func() bool { return grt == rt })
	st, gst := Stat{StatRxNoRoute, 1 << 40}, Stat{}
	check("Stat", st.put, gst.get, func() bool { return gst == st })
	li, gli := LinkInfo{PeerID: 1, PubKey: k, Endpoint: "1.2.3.4:5", LastHandshakeUnixNano: 99, TxBytes: 1 << 33, RxBytes: 2}, LinkInfo{}
	check("LinkInfo", func(b *nlb) { _ = li.put(b) }, gli.get, func() bool { return gli == li })
	ev, gev := Event{Kind: EventLinkHandshake, PeerID: 2, UnixNano: 123456789, Endpoint: "[::1]:60198"}, Event{}
	check("Event", func(b *nlb) { _ = ev.put(b) }, gev.get, func() bool { return gev == ev })
	p, gp := Punt{Ingress: 9, Proto: 2, Src: 1, Dst: 3, TTL: 1, Payload: []byte("hello\x00\x00")}, Punt{}
	check("Punt", p.put, gp.get, func() bool { return reflect.DeepEqual(gp, p) })
	in, gin := Inject{Link: 2, Proto: 1, Dst: 2, TTL: 1, Payload: []byte{1, 2, 3}}, Inject{}
	check("Inject", in.put, gin.get, func() bool { return reflect.DeepEqual(gin, in) })
	// An empty payload must survive as an empty (not erroring) payload.
	gp = Punt{}
	roundTrip(t, Punt{Proto: 1}.put, gp.get)
	if len(gp.Payload) != 0 {
		t.Fatalf("empty punt: %+v", gp)
	}
}

// Every attribute is padded to 4 bytes, and 64-bit attribute payloads land on
// 8-byte boundaries (with a header-only pad attribute where needed), as
// nla_put_64bit does in the kernel.
func TestFramingLayout(t *testing.T) {
	b := newNL(FamilyID, 0, 1, 0, CmdLinkGet, uint8(Version))
	b.u8(AttrPktProto, 1)  // 1 byte payload, padded to 4
	b.u64(AttrTxBytes, 42) // needs a pad attribute first
	b.u32(AttrPeerID, 7)
	b.u64(AttrRxBytes, 43) // (offset now needs no pad, or does; either way aligned)
	raw := b.bytes()
	if len(raw)%4 != 0 || int(nativeEndian.Uint32(raw)) != len(raw) {
		t.Fatalf("length field %d vs %d", nativeEndian.Uint32(raw), len(raw))
	}
	as, err := parseAttrs(raw[HeaderLen:])
	if err != nil {
		t.Fatal(err)
	}
	off := HeaderLen
	for _, a := range as {
		payload := off + nlattrHdrLen
		if (a.typ == AttrTxBytes || a.typ == AttrRxBytes) && payload%8 != 0 {
			t.Fatalf("64-bit attribute %d payload at offset %d is not 8-aligned", a.typ, payload)
		}
		off += align4(nlattrHdrLen + len(a.data))
	}
	if as.u64(AttrTxBytes) != 42 || as.u64(AttrRxBytes) != 43 || as.u32(AttrPeerID) != 7 || as.u8(AttrPktProto) != 1 {
		t.Fatalf("values did not survive: %+v", as)
	}
}

func TestEndpointSockaddr(t *testing.T) {
	for _, ep := range []string{"1.2.3.4:60198", "[2001:db8::1]:1", "[::1]:65535"} {
		sa, err := endpointToSockaddr(ep)
		if err != nil {
			t.Fatal(err)
		}
		if (ep[0] == '[') != (len(sa) == 28) || (ep[0] != '[') != (len(sa) == 16) {
			t.Fatalf("%s: sockaddr length %d", ep, len(sa))
		}
		if got, err := sockaddrToEndpoint(sa); err != nil || got != ep {
			t.Fatalf("%s -> %v %v", ep, got, err)
		}
	}
	for _, bad := range []string{"", "nonsense", "1.2.3.4", "host:80"} {
		if _, err := endpointToSockaddr(bad); err == nil {
			t.Errorf("endpoint %q accepted", bad)
		}
	}
}

func TestErrorMessages(t *testing.T) {
	req := nlmsg{typ: FamilyID, flags: nlmFRequest | nlmFAck, seq: 77}
	// An ACK.
	ms, err := splitMessages(errMessage(req, nil))
	if err != nil || len(ms) != 1 || ms[0].typ != nlmsgError || ms[0].seq != 77 {
		t.Fatalf("ack: %v %+v", err, ms)
	}
	if err := parseErrMessage(ms[0]); err != nil {
		t.Fatalf("ack parsed as error: %v", err)
	}
	// An error keeps its errno and its text (as an extended-ack message).
	ms, _ = splitMessages(errMessage(req, Errorf(CodeExists, "link %d exists", 3)))
	err = parseErrMessage(ms[0])
	if !IsCode(err, CodeExists) || err.(*Error).Msg != "link 3 exists" {
		t.Fatalf("error: %v", err)
	}
	// Garbage never panics.
	for _, g := range [][]byte{nil, {1}, make([]byte, 15), {255, 255, 255, 255, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		_, _ = splitMessages(g)
	}
	if _, err := parseAttrs([]byte{200, 0, 1, 0}); err == nil {
		t.Error("attribute longer than the buffer accepted")
	}
}

// fakeDP is an in-memory Handler.
type fakeDP struct {
	mu         sync.Mutex
	links      map[uint32]LinkAdd
	tuns       map[uint32]*TunInfo
	routes     map[uint32]uint32
	injects    chan Inject
	dev        *DeviceSet
	deviceDels int
	quit       chan struct{}
}

func newFakeDP() *fakeDP {
	return &fakeDP{links: map[uint32]LinkAdd{}, tuns: map[uint32]*TunInfo{}, routes: map[uint32]uint32{}, injects: make(chan Inject, 16), quit: make(chan struct{})}
}

func (f *fakeDP) Hello(Hello) (HelloReply, error) {
	return HelloReply{Version: Version, PID: 99}, nil
}
func (f *fakeDP) DeviceSet(m DeviceSet) (DeviceSetReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dev != nil && *f.dev != m {
		return DeviceSetReply{}, Errorf(CodeExists, "configured differently")
	}
	f.dev = &m
	return DeviceSetReply{PubKey: [32]byte{0xaa}}, nil
}
func (f *fakeDP) Stats() ([]Stat, error) { return []Stat{{StatPuntSent, 5}}, nil }
func (f *fakeDP) DeviceDel() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dev = nil
	f.links, f.tuns, f.routes = map[uint32]LinkAdd{}, map[uint32]*TunInfo{}, map[uint32]uint32{}
	f.deviceDels++
	return nil
}
func (f *fakeDP) Quit() { close(f.quit) }
func (f *fakeDP) LinkAdd(m LinkAdd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if old, ok := f.links[m.PeerID]; ok && old.PubKey != m.PubKey {
		return Errorf(CodeExists, "link %d exists with another key", m.PeerID)
	}
	f.links[m.PeerID] = m
	return nil
}
func (f *fakeDP) LinkDel(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.links[id]; !ok {
		return Errorf(CodeNotFound, "no link %d", id)
	}
	delete(f.links, id)
	return nil
}
func (f *fakeDP) LinkList() ([]LinkInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []LinkInfo
	for id, l := range f.links {
		out = append(out, LinkInfo{PeerID: id, PubKey: l.PubKey, Endpoint: l.Endpoint})
	}
	return out, nil
}
func (f *fakeDP) TunCreate(m TunCreate) (TunCreateReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tuns[m.PeerID]; ok {
		return TunCreateReply{Name: t.Name, Started: t.Started}, nil
	}
	f.tuns[m.PeerID] = &TunInfo{PeerID: m.PeerID, Name: m.Name}
	return TunCreateReply{Name: m.Name}, nil
}
func (f *fakeDP) TunStart(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tuns[id]
	if !ok {
		return Errorf(CodeNotFound, "no tun %d", id)
	}
	t.Started = true
	return nil
}
func (f *fakeDP) TunDestroy(id uint32) error {
	f.mu.Lock()
	delete(f.tuns, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeDP) TunList() ([]TunInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []TunInfo
	for _, t := range f.tuns {
		out = append(out, *t)
	}
	return out, nil
}
func (f *fakeDP) RouteSet(r Route) error {
	f.mu.Lock()
	f.routes[r.Dst] = r.NextHop
	f.mu.Unlock()
	return nil
}
func (f *fakeDP) RouteDel(d uint32) error {
	f.mu.Lock()
	delete(f.routes, d)
	f.mu.Unlock()
	return nil
}
func (f *fakeDP) RouteList() ([]Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Route
	for d, n := range f.routes {
		out = append(out, Route{d, n})
	}
	return out, nil
}
func (f *fakeDP) Inject(m Inject) { f.injects <- m }

func TestClientServer(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	h := newFakeDP()
	srv, err := NewServer(sock, h)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Run()

	punts := make(chan Punt, 4)
	events := make(chan Event, 4)
	cl, err := Dial(sock, func(p Punt) { punts <- p }, func(e Event) { events <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	hr, err := cl.Hello()
	if err != nil || hr.PID != 99 || hr.Configured {
		t.Fatalf("hello: %v %+v", err, hr)
	}
	if srv.Connected() {
		t.Fatal("a session that has not Attached must not be the control plane")
	}
	if err := cl.Attach(); err != nil {
		t.Fatal(err)
	}
	if !srv.Connected() {
		t.Fatal("Attach did not register the session")
	}

	// Device configuration: idempotent when identical, CodeExists when different.
	ds := DeviceSet{LocalID: 1, ListenPort: 60198, MTU: 1416}
	if r, err := cl.DeviceSet(ds); err != nil || r.PubKey != [32]byte{0xaa} {
		t.Fatalf("DeviceSet: %v %+v", err, r)
	}
	if _, err := cl.DeviceSet(ds); err != nil {
		t.Fatalf("repeating an identical DeviceSet must succeed: %v", err)
	}
	ds.MTU = 1400
	if _, err := cl.DeviceSet(ds); !IsCode(err, CodeExists) {
		t.Fatalf("different DeviceSet: want CodeExists, got %v", err)
	}
	if st, err := cl.Stats(); err != nil || len(st) != 1 || st[0] != (Stat{StatPuntSent, 5}) {
		t.Fatalf("Stats: %v %+v", err, st)
	}

	var k1, k2 [32]byte
	k1[0], k2[0] = 1, 2
	if err := cl.LinkAdd(LinkAdd{PeerID: 5, PubKey: k1, Endpoint: "1.1.1.1:1"}); err != nil {
		t.Fatal(err)
	}
	if err := cl.LinkAdd(LinkAdd{PeerID: 5, PubKey: k2}); !IsCode(err, CodeExists) {
		t.Fatalf("want CodeExists, got %v", err)
	}
	if err := cl.LinkDel(99); !IsCode(err, CodeNotFound) {
		t.Fatalf("want CodeNotFound, got %v", err)
	}
	if l, err := cl.LinkList(); err != nil || len(l) != 1 || l[0].PeerID != 5 {
		t.Fatalf("LinkList: %v %+v", err, l)
	}

	if r, err := cl.TunCreate(4, "node-4"); err != nil || r.Name != "node-4" || r.Started {
		t.Fatalf("TunCreate: %v %+v", err, r)
	}
	if err := cl.TunStart(4); err != nil {
		t.Fatal(err)
	}
	if r, _ := cl.TunCreate(4, "node-4"); !r.Started {
		t.Fatal("TunCreate on existing started tun should report Started")
	}
	if err := cl.RouteSet(Route{Dst: 4, NextHop: 5}); err != nil {
		t.Fatal(err)
	}
	if rl, err := cl.RouteList(); err != nil || len(rl) != 1 || rl[0] != (Route{4, 5}) {
		t.Fatalf("RouteList: %v %+v", err, rl)
	}
	if err := cl.RouteDel(4); err != nil {
		t.Fatal(err)
	}
	if err := cl.TunDestroy(4); err != nil {
		t.Fatal(err)
	}

	// An unknown command gets an error reply rather than wedging, and a
	// request missing a required attribute is EINVAL.
	if err := cl.ack(99, nil); !IsCode(err, CodeUnsupported) {
		t.Fatalf("unknown command: %v", err)
	}
	if err := cl.ack(CmdLinkDel, nil); !IsCode(err, CodeInvalid) {
		t.Fatalf("missing attribute: %v", err)
	}

	// DeviceDel returns the data plane to unconfigured, so a different
	// DeviceSet is accepted afterwards (and nothing survives the teardown).
	if err := cl.DeviceDel(); err != nil {
		t.Fatal(err)
	}
	if l, _ := cl.LinkList(); len(l) != 0 {
		t.Fatalf("links survived DeviceDel: %+v", l)
	}
	if _, err := cl.DeviceSet(ds); err != nil {
		t.Fatalf("DeviceSet after DeviceDel: %v", err)
	}

	// Inject: client -> data plane.
	if err := cl.Inject(Inject{Link: 5, Proto: 1, Dst: 5, TTL: 1, Payload: []byte("ping")}); err != nil {
		t.Fatal(err)
	}
	select {
	case in := <-h.injects:
		if in.Link != 5 || string(in.Payload) != "ping" {
			t.Fatalf("inject: %+v", in)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inject not delivered")
	}

	// Event: data plane -> client, ordered with punts.
	srv.Event(Event{Kind: EventLinkHandshake, PeerID: 5, UnixNano: 42, Endpoint: "1.2.3.4:5"})
	select {
	case e := <-events:
		if e.PeerID != 5 || e.Endpoint != "1.2.3.4:5" {
			t.Fatalf("event: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not delivered")
	}

	// Punt: data plane -> client.
	srv.Punt(Punt{Ingress: 5, Proto: 2, Src: 5, Dst: 1, TTL: 1, Payload: []byte("pv")})
	select {
	case p := <-punts:
		if p.Ingress != 5 || p.Proto != 2 || string(p.Payload) != "pv" {
			t.Fatalf("punt: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("punt not delivered")
	}
}

// A dump with many entries arrives as many multipart messages, and an empty
// one as just the terminator.
func TestDumpsAreMultipart(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	h := newFakeDP()
	for i := uint32(0); i < 200; i++ {
		h.links[i] = LinkAdd{PeerID: i, PubKey: [32]byte{byte(i)}, Endpoint: "10.0.0.1:1"}
	}
	srv, err := NewServer(sock, h)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Run()
	cl, err := Dial(sock, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	l, err := cl.LinkList()
	if err != nil || len(l) != 200 {
		t.Fatalf("LinkList: %v, %d entries", err, len(l))
	}
	if tl, err := cl.TunList(); err != nil || len(tl) != 0 {
		t.Fatalf("empty dump: %v %+v", err, tl)
	}
}

func TestQuitRepliesThenCallsHandler(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	h := newFakeDP()
	srv, err := NewServer(sock, h)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Run()
	cl, err := Dial(sock, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if err := cl.Quit(); err != nil {
		t.Fatalf("Quit must be acknowledged before the data plane exits: %v", err)
	}
	select {
	case <-h.quit:
	case <-time.After(2 * time.Second):
		t.Fatal("handler's Quit not called")
	}
}

// Events and punts with no session attached are counted, not queued.
func TestEventsWithoutSessionAreCounted(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	srv, err := NewServer(sock, newFakeDP())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Event(Event{Kind: EventLinkHandshake})
	if srv.EventsDropped() != 1 {
		t.Fatalf("EventsDropped = %d", srv.EventsDropped())
	}
}

func TestSessionReplacementAndLoss(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	srv, err := NewServer(sock, newFakeDP())
	if err != nil {
		t.Fatal(err)
	}
	go srv.Run()

	// No session: punts are dropped and counted, never block.
	srv.Punt(Punt{Proto: 1})
	if srv.PuntDropped() != 1 {
		t.Fatalf("dropped = %d", srv.PuntDropped())
	}

	a, err := Dial(sock, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Attach(); err != nil {
		t.Fatal(err)
	}
	// A tool connecting (never attaching) does not disturb the control plane.
	tool, err := Dial(sock, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Hello(); err != nil {
		t.Fatal(err)
	}
	tool.Close()
	select {
	case <-a.Done():
		t.Fatal("a non-attaching session replaced the control plane")
	case <-time.After(100 * time.Millisecond):
	}
	b, err := Dial(sock, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Attach(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("old session not closed when replaced")
	}
	if _, err := a.Hello(); err == nil {
		t.Fatal("call on a dead session should fail")
	}

	// The data plane going away is seen as session loss.
	srv.Close()
	select {
	case <-b.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session loss not detected")
	}
}

// The userspace data plane answers the genetlink controller's GETFAMILY like
// the kernel would, and the client resolves the family id that way instead of
// assuming it.
func TestFamilyResolution(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dp.sock")
	srv, err := NewServer(sock, newFakeDP())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go srv.Run()
	cl, err := Dial(sock, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if cl.family != FamilyID {
		t.Fatalf("resolved family %d, want %d", cl.family, FamilyID)
	}

	parts, err := cl.request(genlIDCtrl, ctrlCmdGetFamily, ctrlVersion, false, func(b *nlb) error {
		b.str(ctrlAttrFamilyName, FamilyName)
		return nil
	})
	if err != nil || len(parts) != 1 {
		t.Fatalf("GETFAMILY: %v %d", err, len(parts))
	}
	a := parts[0]
	if a.str(ctrlAttrFamilyName) != FamilyName || a.u32(ctrlAttrVersion) != Version {
		t.Fatalf("family info: %+v", a)
	}
	grps, ok := a.get(ctrlAttrMcastGroups)
	if !ok {
		t.Fatal("no multicast groups in the family info")
	}
	entries, err := parseAttrs(grps)
	if err != nil || len(entries) != 1 {
		t.Fatalf("groups: %v %d", err, len(entries))
	}
	g, err := parseAttrs(entries[0].data)
	if err != nil || g.str(ctrlAttrMcastGrpName) != eventsGroupName || g.u32(ctrlAttrMcastGrpID) != eventsGroupID {
		t.Fatalf("events group: %v %+v", err, g)
	}

	// Other families don't exist here, and neither does a melnode request
	// addressed to a family id we never handed out.
	_, err = cl.request(genlIDCtrl, ctrlCmdGetFamily, ctrlVersion, false, func(b *nlb) error {
		b.str(ctrlAttrFamilyName, "wireguard")
		return nil
	})
	if !IsCode(err, CodeNotFound) {
		t.Fatalf("unknown family name: %v", err)
	}
	if _, err = cl.request(FamilyID+1, CmdHello, uint8(Version), false, nil); !IsCode(err, CodeNotFound) {
		t.Fatalf("wrong family id: %v", err)
	}
}
