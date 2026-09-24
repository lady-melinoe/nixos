package dpproto

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestCodecRoundTrip(t *testing.T) {
	var k [32]byte
	for i := range k {
		k[i] = byte(i)
	}
	la := LinkAdd{PeerID: 7, PubKey: k, Endpoint: "[2001:db8::1]:60198"}
	if got, err := UnmarshalLinkAdd(la.Marshal()); err != nil || got != la {
		t.Fatalf("LinkAdd: %v %+v", err, got)
	}
	hr := HelloReply{Version: 1, LocalID: 3, PubKey: k, MTU: 1416, Port: 60198}
	if got, err := UnmarshalHelloReply(hr.Marshal()); err != nil || got != hr {
		t.Fatalf("HelloReply: %v %+v", err, got)
	}
	ll := []LinkInfo{{PeerID: 1, PubKey: k, Endpoint: "1.2.3.4:5", LastHandshakeUnixNano: 99, TxBytes: 1, RxBytes: 2}, {PeerID: 2}}
	if got, err := UnmarshalLinkList(MarshalLinkList(ll)); err != nil || !reflect.DeepEqual(got, ll) {
		t.Fatalf("LinkList: %v %+v", err, got)
	}
	tl := []TunInfo{{PeerID: 4, Name: "node-4", MTU: 1416, Started: true}, {PeerID: 5, Name: "node-5"}}
	if got, err := UnmarshalTunList(MarshalTunList(tl)); err != nil || !reflect.DeepEqual(got, tl) {
		t.Fatalf("TunList: %v %+v", err, got)
	}
	rl := []Route{{1, 2}, {3, 4}}
	if got, err := UnmarshalRouteList(MarshalRouteList(rl)); err != nil || !reflect.DeepEqual(got, rl) {
		t.Fatalf("RouteList: %v %+v", err, got)
	}
	p := Punt{Ingress: 9, Proto: 2, Src: 1, Dst: 3, TTL: 1, Payload: []byte("hello\x00\x00")}
	msg := p.MarshalMessage()
	if Type(msg[0]) != TypePunt {
		t.Fatalf("punt type %d", msg[0])
	}
	if got, err := UnmarshalPunt(msg[HeaderLen:]); err != nil || !reflect.DeepEqual(got, p) {
		t.Fatalf("Punt: %v %+v", err, got)
	}
	in := Inject{Link: 2, Proto: 1, Dst: 2, TTL: 1, Payload: []byte{1, 2, 3}}
	if got, err := UnmarshalInject(in.Marshal()); err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("Inject: %v %+v", err, got)
	}
	// An empty payload must survive as an empty (not erroring) payload.
	if got, err := UnmarshalPunt(Punt{Proto: 1}.MarshalMessage()[HeaderLen:]); err != nil || len(got.Payload) != 0 {
		t.Fatalf("empty punt: %v %+v", err, got)
	}
}

func TestCodecRejectsGarbage(t *testing.T) {
	if _, err := UnmarshalLinkAdd([]byte{1, 2}); err == nil {
		t.Error("truncated LinkAdd accepted")
	}
	if _, err := UnmarshalID([]byte{0, 0, 0, 1, 9}); err == nil {
		t.Error("trailing bytes accepted")
	}
	if _, err := UnmarshalLinkList([]byte{0, 0, 0, 5}); err == nil {
		t.Error("list with missing entries accepted")
	}
}

// fakeDP is an in-memory Handler.
type fakeDP struct {
	mu      sync.Mutex
	links   map[uint32]LinkAdd
	tuns    map[uint32]*TunInfo
	routes  map[uint32]uint32
	injects chan Inject
}

func newFakeDP() *fakeDP {
	return &fakeDP{links: map[uint32]LinkAdd{}, tuns: map[uint32]*TunInfo{}, routes: map[uint32]uint32{}, injects: make(chan Inject, 16)}
}

func (f *fakeDP) Hello(Hello) (HelloReply, error) {
	return HelloReply{Version: Version, LocalID: 1, MTU: 1416}, nil
}
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
func (f *fakeDP) TunCreate(id uint32) (TunCreateReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tuns[id]; ok {
		return TunCreateReply{Name: t.Name, Started: t.Started}, nil
	}
	f.tuns[id] = &TunInfo{PeerID: id, Name: "node-x"}
	return TunCreateReply{Name: "node-x"}, nil
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
	cl, err := Dial(sock, func(p Punt) { punts <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	hr, err := cl.Hello()
	if err != nil || hr.LocalID != 1 || hr.MTU != 1416 {
		t.Fatalf("hello: %v %+v", err, hr)
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

	if r, err := cl.TunCreate(4); err != nil || r.Name != "node-x" || r.Started {
		t.Fatalf("TunCreate: %v %+v", err, r)
	}
	if err := cl.TunStart(4); err != nil {
		t.Fatal(err)
	}
	if r, _ := cl.TunCreate(4); !r.Started {
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

	// Unknown request type gets an error reply rather than wedging.
	if _, err := cl.Call(Type(0x42), nil); !IsCode(err, CodeUnsupported) {
		t.Fatalf("unknown type: %v", err)
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

	a, err := Dial(sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Hello(); err != nil {
		t.Fatal(err)
	}
	b, err := Dial(sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Hello(); err != nil {
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
