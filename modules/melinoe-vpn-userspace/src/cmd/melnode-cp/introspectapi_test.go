package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"melnode/dpproto"
)

// countingDP answers the introspection reads with fixed data and counts
// every call; fail makes LinkList error.
type countingDP struct {
	stubDP
	mu    sync.Mutex
	calls int
	fail  bool
}

func (d *countingDP) hit() (fail bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return d.fail
}

func (d *countingDP) n() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *countingDP) setFail(f bool) {
	d.mu.Lock()
	d.fail = f
	d.mu.Unlock()
}

func (d *countingDP) LinkList() ([]dpproto.LinkInfo, error) {
	if d.hit() {
		return nil, errors.New("injected failure")
	}
	return []dpproto.LinkInfo{{PeerID: 2, TxBytes: 7}}, nil
}

func (d *countingDP) TunList() ([]dpproto.TunInfo, error) {
	d.hit()
	return []dpproto.TunInfo{{PeerID: 2, Name: "node-2", MTU: 1416, Started: true}}, nil
}

func (d *countingDP) RouteList() ([]dpproto.Route, error) {
	d.hit()
	return []dpproto.Route{{Dst: 2, NextHop: 2}}, nil
}

func (d *countingDP) Stats() ([]dpproto.Stat, error) {
	d.hit()
	return []dpproto.Stat{{ID: dpproto.StatPuntSent, Value: 3}}, nil
}

func newTestIntrospectAPI(t *testing.T) (*introspectAPI, *countingDP) {
	t.Helper()
	n := newTestNode(t)
	n.links[2] = newLink(n, 2, [32]byte{}, "", 0)
	dp := &countingDP{}
	n.dpc.Store(&dpHandle{dp})
	// An hour: only the refreshes a test asks for happen.
	a, err := newIntrospectAPI(n.pathVector, "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a.Start()
	t.Cleanup(a.Stop)
	return a, dp
}

func getJSON(t *testing.T, a *introspectAPI, path string, into any) http.Header {
	t.Helper()
	resp, err := http.Get("http://" + a.listener.Addr().String() + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp.Header
}

// TCP requests are served from the snapshot: however many arrive, none
// reaches the data plane.
func TestIntrospectTCPServesSnapshot(t *testing.T) {
	a, dp := newTestIntrospectAPI(t)
	before := dp.n()
	if before == 0 {
		t.Fatal("Start didn't read the data plane for the first snapshot")
	}

	for i := 0; i < 10; i++ {
		for _, path := range readPaths {
			var v any
			getJSON(t, a, path, &v)
		}
	}
	if got := dp.n(); got != before {
		t.Fatalf("requests reached the data plane: %d calls, want %d", got, before)
	}

	var tuns []tunInfo
	getJSON(t, a, "/tuns", &tuns)
	if len(tuns) != 1 || tuns[0].Name != "node-2" || tuns[0].NextHop == nil || *tuns[0].NextHop != 2 {
		t.Fatalf("/tuns = %+v, want node-2 via 2", tuns)
	}
	var sum summaryInfo
	h := getJSON(t, a, "/summary", &sum)
	if h.Get("Age") == "" {
		t.Error("no Age header")
	}
	at, err := time.Parse(time.RFC3339Nano, h.Get("X-Melnode-Generated-At"))
	if err != nil || !at.Equal(sum.GeneratedAt) {
		t.Errorf("X-Melnode-Generated-At %q vs generated_at %v", h.Get("X-Melnode-Generated-At"), sum.GeneratedAt)
	}
	if sum.Tuns != 1 || sum.LinksTotal != 1 || !sum.DataplaneAttached {
		t.Errorf("/summary = %+v", sum)
	}
}

// A refresh whose data plane read fails keeps the previous snapshot rather
// than serving one with the data plane's details missing; the next good
// one replaces it. Detaching is a real state, not a failure.
func TestIntrospectRefreshKeepsSnapshotOnFailure(t *testing.T) {
	a, dp := newTestIntrospectAPI(t)
	first := a.snap.Load()

	dp.setFail(true)
	a.refresh()
	if a.snap.Load() != first {
		t.Fatal("failed refresh replaced the snapshot")
	}

	dp.setFail(false)
	a.refresh()
	if a.snap.Load() == first {
		t.Fatal("good refresh didn't replace the snapshot")
	}

	a.pv.node.dpc.Store(nil)
	a.refresh()
	var info dataplaneInfo
	getJSON(t, a, "/dataplane", &info)
	if info.Attached {
		t.Fatal("snapshot still says attached after detaching")
	}
}

// The refresher waits for a query holding the data plane gate instead of
// building a snapshot without the data plane's details.
func TestIntrospectRefreshWaitsForGate(t *testing.T) {
	a, _ := newTestIntrospectAPI(t)
	_, release := a.pv.node.introspectDP()
	first := a.snap.Load()

	done := make(chan struct{})
	go func() {
		a.refresh()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("refresh ran while the gate was held")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-done

	if s := a.snap.Load(); s == first {
		t.Fatal("refresh didn't store a snapshot")
	}
	var tuns []tunInfo
	getJSON(t, a, "/tuns", &tuns)
	if len(tuns) != 1 {
		t.Fatalf("snapshot built without the data plane: /tuns = %+v", tuns)
	}
}
