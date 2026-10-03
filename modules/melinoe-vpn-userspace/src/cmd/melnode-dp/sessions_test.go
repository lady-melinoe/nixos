package main

import (
	"path/filepath"
	"testing"
	"time"

	"melnode/dpproto"
)

func startTestServer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dp.sock")
	srv, err := dpproto.NewServer(path, func(s *dpproto.Session) dpproto.Handler {
		return &ctlHandler{sess: s}
	})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Run()
	t.Cleanup(srv.Close)
	return path
}

func dialTest(t *testing.T, path string) *dpproto.Client {
	t.Helper()
	cl, err := dpproto.Dial(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return cl
}

func testParams(id uint32, port uint16, keyByte byte) dpproto.DeviceSet {
	var k [32]byte
	for i := range k {
		k[i] = keyByte
	}
	return dpproto.DeviceSet{LocalID: id, PrivateKey: k, ListenPort: port, MTU: 1416}
}

func TestInstancesAreIndependent(t *testing.T) {
	path := startTestServer(t)
	a, b := dialTest(t, path), dialTest(t, path)

	ra, err := a.DeviceSet(testParams(1, 47811, 7))
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.DeviceSet(testParams(2, 47812, 9))
	if err != nil {
		t.Fatal(err)
	}
	if ra.PubKey == rb.PubKey {
		t.Fatal("instances share a public key")
	}

	if err := a.RouteSet([]dpproto.Route{{Dst: 5, NextHop: 6}}); err != nil {
		t.Fatal(err)
	}
	ra2, err := a.RouteList()
	if err != nil || len(ra2) != 1 {
		t.Fatalf("a routes = %v, %v", ra2, err)
	}
	rb2, err := b.RouteList()
	if err != nil || len(rb2) != 0 {
		t.Fatalf("b routes = %v, %v", rb2, err)
	}

	if _, err := a.DeviceSet(testParams(1, 47811, 7)); err != nil {
		t.Fatalf("identical DeviceSet: %v", err)
	}
	if _, err := a.DeviceSet(testParams(3, 47811, 7)); !dpproto.IsCode(err, dpproto.CodeExists) {
		t.Fatalf("changed DeviceSet: %v, want exists", err)
	}
}

func TestClosingSessionFreesPort(t *testing.T) {
	path := startTestServer(t)
	a := dialTest(t, path)
	if _, err := a.DeviceSet(testParams(1, 47813, 7)); err != nil {
		t.Fatal(err)
	}

	b := dialTest(t, path)
	done := make(chan error, 1)
	go func() {
		_, err := b.DeviceSet(testParams(2, 47813, 9))
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	a.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("DeviceSet after peer session closed: %v", err)
		}
	case <-time.After(2 * restartGrace):
		t.Fatal("DeviceSet did not complete")
	}
}

func TestRoutesReplaceAndValidate(t *testing.T) {
	path := startTestServer(t)
	a := dialTest(t, path)
	if _, err := a.DeviceSet(testParams(1, 47814, 7)); err != nil {
		t.Fatal(err)
	}
	if err := a.RouteSet([]dpproto.Route{{Dst: 5, NextHop: 6}, {Dst: 5, NextHop: 7}}); !dpproto.IsCode(err, dpproto.CodeInvalid) {
		t.Fatalf("duplicate dst: %v", err)
	}
	if err := a.RouteSet([]dpproto.Route{{Dst: 5, NextHop: 6}}); err != nil {
		t.Fatal(err)
	}
	if err := a.RouteSet(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.RouteList(); len(got) != 0 {
		t.Fatalf("routes after clear = %v", got)
	}
}
