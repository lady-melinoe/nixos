package dpproto

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func roundTrip(t *testing.T, put func(*nlb), get func(attrs) error) {
	t.Helper()
	b := newNL(FamilyID, 0, 1, 0, CmdLinkSet, uint8(Version))
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
		want := LinkSet{PeerID: 7, PubKey: k, Endpoint: "[2001:db8::1]:60198"}
		var got LinkSet
		roundTrip(t, func(b *nlb) {
			if err := want.put(b); err != nil {
				t.Fatal(err)
			}
		}, got.get)
		if got != want {
			t.Fatalf("LinkSet: %+v", got)
		}
	}
	{
		want := LinkSet{PeerID: 7, PubKey: k}
		var got LinkSet
		roundTrip(t, func(b *nlb) { _ = want.put(b) }, got.get)
		if got != want {
			t.Fatalf("listen-only LinkSet: %+v", got)
		}
	}
	check := func(name string, put func(*nlb), get func(attrs) error, eq func() bool) {
		t.Helper()
		roundTrip(t, put, get)
		if !eq() {
			t.Fatalf("%s did not round trip", name)
		}
	}
	ds, gds := DeviceSet{LocalID: 3, PrivateKey: k, ListenPort: 60198, Fwmark: 0x1234, MTU: 1416}, DeviceSet{}
	check("DeviceSet", ds.put, gds.get, func() bool { return gds == ds })
	dr, gdr := DeviceSetReply{PubKey: k}, DeviceSetReply{}
	check("DeviceSetReply", dr.put, gdr.get, func() bool { return gdr == dr })
	ts, gts := TunSet{PeerID: 4, Name: "node-4"}, TunSet{}
	check("TunSet", ts.put, gts.get, func() bool { return gts == ts })
	ti, gti := TunInfo{PeerID: 4, Name: "node-4", IfIndex: 17}, TunInfo{}
	check("TunInfo", ti.put, gti.get, func() bool { return gti == ti })
	rt, grt := Route{Dst: 3, NextHop: 4}, Route{}
	check("Route", rt.put, grt.get, func() bool { return grt == rt })
	st, gst := Stat{StatRxNoRoute, 1 << 40}, Stat{}
	check("Stat", st.put, gst.get, func() bool { return gst == st })
	li, gli := LinkInfo{PeerID: 1, PubKey: k, Endpoint: "1.2.3.4:5", LastHandshakeUnixNano: 99, TxBytes: 1 << 33, RxBytes: 2}, LinkInfo{}
	check("LinkInfo", func(b *nlb) { _ = li.put(b) }, gli.get, func() bool { return gli == li })
	p, gp := Punt{Ingress: 9, Proto: 2, Src: 1, Dst: 3, TTL: 1, Payload: []byte("hello\x00\x00")}, Punt{}
	check("Punt", p.put, gp.get, func() bool { return reflect.DeepEqual(gp, p) })
	in, gin := Inject{Link: 2, Proto: 1, Dst: 2, TTL: 1, Payload: []byte{1, 2, 3}}, Inject{}
	check("Inject", in.put, gin.get, func() bool { return reflect.DeepEqual(gin, in) })
	routes := []Route{{Dst: 1, NextHop: 2}, {Dst: 3, NextHop: 2}, {Dst: 9, NextHop: 4}}
	var gotRoutes []Route
	roundTrip(t, func(b *nlb) { putRoutes(b, routes) }, func(a attrs) (err error) { gotRoutes, err = getRoutes(a); return })
	if !reflect.DeepEqual(gotRoutes, routes) {
		t.Fatalf("route table: %+v", gotRoutes)
	}
	roundTrip(t, func(b *nlb) { putRoutes(b, nil) }, func(a attrs) (err error) { gotRoutes, err = getRoutes(a); return })
	if len(gotRoutes) != 0 {
		t.Fatalf("empty route table: %+v", gotRoutes)
	}
	if _, err := getRoutes(attrs{}); !IsCode(err, CodeInvalid) {
		t.Fatalf("missing route table: %v", err)
	}
	gp = Punt{}
	roundTrip(t, Punt{Proto: 1}.put, gp.get)
	if len(gp.Payload) != 0 {
		t.Fatalf("empty punt: %+v", gp)
	}
}

func TestFramingLayout(t *testing.T) {
	b := newNL(FamilyID, 0, 1, 0, CmdLinkGet, uint8(Version))
	b.u8(AttrPktProto, 1)
	b.u64(AttrTxBytes, 42)
	b.u32(AttrPeerID, 7)
	b.u64(AttrRxBytes, 43)
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
	ms, err := splitMessages(errMessage(req, nil))
	if err != nil || len(ms) != 1 || ms[0].typ != nlmsgError || ms[0].seq != 77 {
		t.Fatalf("ack: %v %+v", err, ms)
	}
	if err := parseErrMessage(ms[0]); err != nil {
		t.Fatalf("ack parsed as error: %v", err)
	}
	ms, _ = splitMessages(errMessage(req, Errorf(CodeExists, "link %d exists", 3)))
	err = parseErrMessage(ms[0])
	if !IsCode(err, CodeExists) || err.(*Error).Msg != "link 3 exists" {
		t.Fatalf("error: %v", err)
	}
	for _, g := range [][]byte{nil, {1}, make([]byte, 15), {255, 255, 255, 255, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		_, _ = splitMessages(g)
	}
	if _, err := parseAttrs([]byte{200, 0, 1, 0}); err == nil {
		t.Error("attribute longer than the buffer accepted")
	}
}

type dpRegistry struct {
	mu     sync.Mutex
	closed int
}

type fakeDP struct {
	reg     *dpRegistry
	sess    *Session
	mu      sync.Mutex
	links   map[uint32]LinkSet
	tuns    map[uint32]TunInfo
	routes  map[uint32]uint32
	injects chan Inject
	dev     *DeviceSet
}

func newFakeDP(reg *dpRegistry, sess *Session) *fakeDP {
	return &fakeDP{reg: reg, sess: sess, links: map[uint32]LinkSet{}, tuns: map[uint32]TunInfo{}, routes: map[uint32]uint32{}, injects: make(chan Inject, 16)}
}

func (f *fakeDP) DeviceSet(m DeviceSet) (DeviceSetReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dev != nil && *f.dev != m {
		return DeviceSetReply{}, Errorf(CodeExists, "configured differently")
	}
	f.dev = &m
	return DeviceSetReply{PubKey: [32]byte{0xaa, byte(m.LocalID)}}, nil
}
func (f *fakeDP) Stats() ([]Stat, error) { return []Stat{{StatPuntSent, 5}}, nil }
func (f *fakeDP) LinkSet(m LinkSet) error {
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
func (f *fakeDP) TunSet(m TunSet) (TunInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tuns[m.PeerID]; ok {
		if t.Name != m.Name {
			return TunInfo{}, Errorf(CodeExists, "tun %d is named %q", m.PeerID, t.Name)
		}
		return t, nil
	}
	t := TunInfo{PeerID: m.PeerID, Name: m.Name, IfIndex: 100 + m.PeerID}
	f.tuns[m.PeerID] = t
	return t, nil
}
func (f *fakeDP) TunDel(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tuns[id]; !ok {
		return Errorf(CodeNotFound, "no tun %d", id)
	}
	delete(f.tuns, id)
	return nil
}
func (f *fakeDP) TunList() ([]TunInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []TunInfo
	for _, t := range f.tuns {
		out = append(out, t)
	}
	return out, nil
}
func (f *fakeDP) RouteSet(rs []Route) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := map[uint32]uint32{}
	for _, r := range rs {
		if _, dup := next[r.Dst]; dup {
			return Errorf(CodeInvalid, "duplicate destination %d", r.Dst)
		}
		next[r.Dst] = r.NextHop
	}
	f.routes = next
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
func (f *fakeDP) Close() {
	f.reg.mu.Lock()
	f.reg.closed++
	f.reg.mu.Unlock()
}

func (r *dpRegistry) closedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

type testServer struct {
	srv  *Server
	sock string
	reg  *dpRegistry
	mu   sync.Mutex
	dps  []*fakeDP
}

func (ts *testServer) dp(i int) *fakeDP {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.dps[i]
}

func startServer(t *testing.T, prepare func(*fakeDP)) *testServer {
	t.Helper()
	ts := &testServer{sock: filepath.Join(t.TempDir(), "dp.sock"), reg: &dpRegistry{}}
	srv, err := NewServer(ts.sock, func(s *Session) Handler {
		f := newFakeDP(ts.reg, s)
		if prepare != nil {
			prepare(f)
		}
		ts.mu.Lock()
		ts.dps = append(ts.dps, f)
		ts.mu.Unlock()
		return f
	})
	if err != nil {
		t.Fatal(err)
	}
	ts.srv = srv
	go srv.Run()
	t.Cleanup(srv.Close)
	return ts
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClientServer(t *testing.T) {
	ts := startServer(t, nil)
	punts := make(chan Punt, 4)
	cl, err := Dial(ts.sock, func(p Punt) { punts <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	waitFor(t, "session", func() bool { return ts.srv.Sessions() == 1 })
	h := ts.dp(0)

	ds := DeviceSet{LocalID: 1, ListenPort: 60198, MTU: 1416}
	if r, err := cl.DeviceSet(ds); err != nil || r.PubKey != [32]byte{0xaa, 1} {
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
	if err := cl.LinkSet(LinkSet{PeerID: 5, PubKey: k1, Endpoint: "1.1.1.1:1"}); err != nil {
		t.Fatal(err)
	}
	if err := cl.LinkSet(LinkSet{PeerID: 5, PubKey: k2}); !IsCode(err, CodeExists) {
		t.Fatalf("want CodeExists, got %v", err)
	}
	if err := cl.LinkDel(99); !IsCode(err, CodeNotFound) {
		t.Fatalf("want CodeNotFound, got %v", err)
	}
	if l, err := cl.LinkList(); err != nil || len(l) != 1 || l[0].PeerID != 5 {
		t.Fatalf("LinkList: %v %+v", err, l)
	}

	if r, err := cl.TunSet(TunSet{PeerID: 4, Name: "node-4"}); err != nil || r.Name != "node-4" || r.IfIndex != 104 {
		t.Fatalf("TunSet: %v %+v", err, r)
	}
	if _, err := cl.TunSet(TunSet{PeerID: 4, Name: "node-4"}); err != nil {
		t.Fatalf("repeating an identical TunSet must succeed: %v", err)
	}
	if _, err := cl.TunSet(TunSet{PeerID: 4, Name: "other"}); !IsCode(err, CodeExists) {
		t.Fatalf("renaming a tun: want CodeExists, got %v", err)
	}
	if tl, err := cl.TunList(); err != nil || len(tl) != 1 || tl[0].IfIndex != 104 {
		t.Fatalf("TunList: %v %+v", err, tl)
	}

	if err := cl.RouteSet([]Route{{Dst: 4, NextHop: 5}, {Dst: 6, NextHop: 5}}); err != nil {
		t.Fatal(err)
	}
	if rl, err := cl.RouteList(); err != nil || len(rl) != 2 {
		t.Fatalf("RouteList: %v %+v", err, rl)
	}
	if err := cl.RouteSet([]Route{{Dst: 7, NextHop: 5}}); err != nil {
		t.Fatal(err)
	}
	if rl, err := cl.RouteList(); err != nil || len(rl) != 1 || rl[0] != (Route{7, 5}) {
		t.Fatalf("RouteSet must replace the whole table: %v %+v", err, rl)
	}
	if err := cl.RouteSet([]Route{{Dst: 7, NextHop: 5}, {Dst: 7, NextHop: 6}}); !IsCode(err, CodeInvalid) {
		t.Fatalf("duplicate destination: %v", err)
	}
	if err := cl.RouteSet(nil); err != nil {
		t.Fatal(err)
	}
	if rl, err := cl.RouteList(); err != nil || len(rl) != 0 {
		t.Fatalf("an empty table must clear the routes: %v %+v", err, rl)
	}
	if err := cl.TunDel(4); err != nil {
		t.Fatal(err)
	}
	if err := cl.TunDel(4); !IsCode(err, CodeNotFound) {
		t.Fatalf("second TunDel: %v", err)
	}

	if err := cl.ack(99, nil); !IsCode(err, CodeUnsupported) {
		t.Fatalf("unknown command: %v", err)
	}
	if err := cl.ack(CmdLinkDel, nil); !IsCode(err, CodeInvalid) {
		t.Fatalf("missing attribute: %v", err)
	}
	if err := cl.ack(CmdRouteSet, nil); !IsCode(err, CodeInvalid) {
		t.Fatalf("ROUTE_SET without a table: %v", err)
	}

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

	h.sess.Punt(Punt{Ingress: 5, Proto: 2, Src: 5, Dst: 1, TTL: 1, Payload: []byte("pv")})
	select {
	case p := <-punts:
		if p.Ingress != 5 || p.Proto != 2 || string(p.Payload) != "pv" {
			t.Fatalf("punt: %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("punt not delivered")
	}
	if h.sess.PuntsSent() != 1 || h.sess.PuntDropped() != 0 {
		t.Fatalf("punt counters: sent %d dropped %d", h.sess.PuntsSent(), h.sess.PuntDropped())
	}
}

func TestSessionsAreIndependent(t *testing.T) {
	ts := startServer(t, nil)
	punts := [2]chan Punt{make(chan Punt, 4), make(chan Punt, 4)}
	var cls [2]*Client
	for i := range cls {
		cl, err := Dial(ts.sock, func(p Punt) { punts[i] <- p })
		if err != nil {
			t.Fatal(err)
		}
		cls[i] = cl
		waitFor(t, "session", func() bool { return ts.srv.Sessions() == i+1 })
	}
	defer cls[1].Close()

	for i, cl := range cls {
		r, err := cl.DeviceSet(DeviceSet{LocalID: uint32(i + 1), ListenPort: uint16(60000 + i), MTU: 1416})
		if err != nil || r.PubKey[1] != byte(i+1) {
			t.Fatalf("session %d DeviceSet: %v %+v", i, err, r)
		}
		if err := cl.LinkSet(LinkSet{PeerID: uint32(10 + i), PubKey: [32]byte{byte(i + 1)}}); err != nil {
			t.Fatal(err)
		}
		if err := cl.RouteSet([]Route{{Dst: uint32(20 + i), NextHop: uint32(10 + i)}}); err != nil {
			t.Fatal(err)
		}
	}
	for i, cl := range cls {
		l, err := cl.LinkList()
		if err != nil || len(l) != 1 || l[0].PeerID != uint32(10+i) {
			t.Fatalf("session %d sees %+v (%v)", i, l, err)
		}
		rl, err := cl.RouteList()
		if err != nil || len(rl) != 1 || rl[0].Dst != uint32(20+i) {
			t.Fatalf("session %d routes %+v (%v)", i, rl, err)
		}
	}

	ts.dp(0).sess.Punt(Punt{Proto: 2, Payload: []byte("a")})
	ts.dp(1).sess.Punt(Punt{Proto: 2, Payload: []byte("b")})
	for i, want := range []string{"a", "b"} {
		select {
		case p := <-punts[i]:
			if string(p.Payload) != want {
				t.Fatalf("session %d got punt %q", i, p.Payload)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("session %d punt not delivered", i)
		}
		select {
		case p := <-punts[i]:
			t.Fatalf("session %d got a second punt %q", i, p.Payload)
		case <-time.After(50 * time.Millisecond):
		}
	}

	cls[0].Close()
	waitFor(t, "first session teardown", func() bool { return ts.reg.closedCount() == 1 })
	if ts.srv.Sessions() != 1 {
		t.Fatalf("sessions after one close: %d", ts.srv.Sessions())
	}
	if l, err := cls[1].LinkList(); err != nil || len(l) != 1 || l[0].PeerID != 11 {
		t.Fatalf("closing one session disturbed the other: %v %+v", err, l)
	}
	select {
	case <-cls[1].Done():
		t.Fatal("the surviving session was closed")
	default:
	}
}

func TestPuntAfterSessionCloseIsCounted(t *testing.T) {
	ts := startServer(t, nil)
	cl, err := Dial(ts.sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "session", func() bool { return ts.srv.Sessions() == 1 })
	sess := ts.dp(0).sess
	cl.Close()
	waitFor(t, "teardown", func() bool { return ts.reg.closedCount() == 1 })
	sess.Punt(Punt{Proto: 1})
	if sess.PuntDropped() != 1 {
		t.Fatalf("dropped = %d", sess.PuntDropped())
	}
}

func TestServerCloseEndsSessions(t *testing.T) {
	ts := startServer(t, nil)
	var cls [3]*Client
	for i := range cls {
		cl, err := Dial(ts.sock, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()
		cls[i] = cl
	}
	waitFor(t, "sessions", func() bool { return ts.srv.Sessions() == 3 })
	ts.srv.Close()
	if n := ts.reg.closedCount(); n != 3 {
		t.Fatalf("handlers closed: %d", n)
	}
	for i, cl := range cls {
		select {
		case <-cl.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("client %d did not notice the server going away", i)
		}
	}
}

func TestDumpsAreMultipart(t *testing.T) {
	ts := startServer(t, func(f *fakeDP) {
		for i := uint32(0); i < 200; i++ {
			f.links[i] = LinkSet{PeerID: i, PubKey: [32]byte{byte(i)}, Endpoint: "10.0.0.1:1"}
		}
	})
	cl, err := Dial(ts.sock, nil)
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

func TestFamilyResolution(t *testing.T) {
	ts := startServer(t, nil)
	cl, err := Dial(ts.sock, nil)
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

	_, err = cl.request(genlIDCtrl, ctrlCmdGetFamily, ctrlVersion, false, func(b *nlb) error {
		b.str(ctrlAttrFamilyName, "wireguard")
		return nil
	})
	if !IsCode(err, CodeNotFound) {
		t.Fatalf("unknown family name: %v", err)
	}
	if _, err = cl.request(FamilyID+1, CmdStatsGet, uint8(Version), false, nil); !IsCode(err, CodeNotFound) {
		t.Fatalf("wrong family id: %v", err)
	}
}
