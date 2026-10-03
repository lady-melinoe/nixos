package dpproto

import (
	"os"
	"testing"
)

func TestKernelInstanceLifecycle(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	a, err := DialKernel(nil)
	if err != nil {
		t.Skipf("melnode kernel module not available: %v", err)
	}
	defer a.Close()

	var key [32]byte
	key[0] = 1
	params := DeviceSet{LocalID: 1, PrivateKey: key, ListenPort: 47901, MTU: 1416}
	first, err := a.DeviceSet(params)
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.DeviceSet(params)
	if err != nil || again.PubKey != first.PubKey {
		t.Fatalf("repeat DeviceSet = %v, %v", again, err)
	}
	changed := params
	changed.MTU = 1300
	if _, err := a.DeviceSet(changed); !IsCode(err, CodeExists) {
		t.Fatalf("changed DeviceSet: %v, want exists", err)
	}

	b, err := DialKernel(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if routes, err := b.RouteList(); !IsCode(err, 19) || routes != nil {
		t.Fatalf("second socket saw instance state: %v, %v", routes, err)
	}

	if err := a.RouteSet([]Route{{Dst: 5, NextHop: 6}, {Dst: 5, NextHop: 7}}); !IsCode(err, CodeInvalid) {
		t.Fatalf("duplicate route: %v", err)
	}
	if err := a.RouteSet([]Route{{Dst: 5, NextHop: 6}}); err != nil {
		t.Fatal(err)
	}
	if got, err := a.RouteList(); err != nil || len(got) != 1 {
		t.Fatalf("routes = %v, %v", got, err)
	}
	if err := a.RouteSet(nil); err != nil {
		t.Fatal(err)
	}
}
