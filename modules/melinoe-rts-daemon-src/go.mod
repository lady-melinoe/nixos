module ctdemo

go 1.26.5

require (
	github.com/cilium/ebpf v0.22.0
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/sys v0.43.0
)

require github.com/vishvananda/netns v0.0.5 // indirect

// bpf2go is invoked only via the go:generate directive in main.go, never
// imported by any package here -- without a `tool` line, `go mod vendor`
// has no reason to vendor github.com/cilium/ebpf/cmd/bpf2go, and
// buildGoModule always builds with -mod=vendor, so `go generate` would
// fail to find it.
tool github.com/cilium/ebpf/cmd/bpf2go
