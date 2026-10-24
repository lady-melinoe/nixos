// melnode-dpctl drives the melnode data plane directly, without the control plane: bring it up,
// add links, devices and routes, inject control-plane packets, watch the counters and the packets
// punted to userspace. It is the tool for debugging and for the adversarial tests, which want
// exact control over what the data plane is given.
//
//	melnode-dpctl [-object FILE] [-pin DIR] COMMAND [ARGS]
//
// Commands: up, down, links, link-set, link-del, tuns, tun-set, tun-del, routes, route-set,
// inject, stats, punts.
package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"melnode/dpproto"
	"melnode/ebpfdp"
)

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "melnode-dpctl: "+format+"\n", args...)
	os.Exit(1)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: melnode-dpctl [-object FILE] [-pin DIR] COMMAND [ARGS]

  up -id N -port P -key FILE [-mtu M] [-fwmark F] [-hub NAME] [-timescale S] [-underload N]
                                 create the data plane and leave it running
  down                           tear it down
  links                          list links
  link-set ID PUBKEY [ENDPOINT]  add a link (or change its endpoint)
  link-del ID                    remove a link
  tuns | tun-set ID NAME | tun-del ID
  routes | route-set DST:NEXTHOP...   replace the whole route table
  inject LINK PROTO DST TTL HEX  send a control-plane packet
  stats [-all]                   counters (non-zero ones unless -all)
  punts [-n COUNT] [-t SECONDS]  print punted packets as JSON lines
`)
	os.Exit(2)
}

func parseID(s string) uint32 {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		die("bad number %q", s)
	}
	return uint32(v)
}

func readKey(path string) [32]byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		die("%v", err)
	}
	return parseKey(strings.TrimSpace(string(raw)))
}

func parseKey(s string) [32]byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		die("bad key %q", s)
	}
	return [32]byte(b)
}

func main() {
	object := flag.String("object", os.Getenv("MELNODE_OBJECT"), "the melnode eBPF object (default $MELNODE_OBJECT)")
	pin := flag.String("pin", ebpfdp.DefaultPinDir, "where the data plane is pinned")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 || *object == "" {
		usage()
	}
	cmd, args := args[0], args[1:]

	var hub string
	var scale uint64
	var upFlags *flag.FlagSet
	var id, port, mtu, fwmark, underload uint
	var keyFile string
	if cmd == "up" {
		upFlags = flag.NewFlagSet("up", flag.ExitOnError)
		upFlags.UintVar(&id, "id", 0, "node id")
		upFlags.UintVar(&port, "port", 0, "UDP port")
		upFlags.UintVar(&mtu, "mtu", 1416, "MTU of the per-peer devices")
		upFlags.UintVar(&fwmark, "fwmark", 0, "fwmark for the UDP sockets")
		upFlags.StringVar(&keyFile, "key", "", "file with the base64 private key")
		upFlags.StringVar(&hub, "hub", "node-hub", "name of the hub device")
		upFlags.Uint64Var(&scale, "timescale", 1, "divide the protocol timers (tests)")
		upFlags.UintVar(&underload, "underload", 0, "handshake messages per second that mean 'under load' (tests)")
		_ = upFlags.Parse(args)
		args = upFlags.Args()
	}

	var puntLimit int64
	var puntCount atomic.Int64
	puntsDone := make(chan struct{})
	var puntsOnce sync.Once
	dp, err := ebpfdp.Open(ebpfdp.Config{
		ObjectPath: *object, PinDir: *pin, HubName: hub, TimeScale: scale, UnderLoadThreshold: uint32(underload),
		Logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
		OnPunt: func(p dpproto.Punt) {
			b, _ := json.Marshal(map[string]any{
				"link": p.Ingress, "proto": p.Proto, "src": p.Src, "dst": p.Dst, "ttl": p.TTL,
				"len": len(p.Payload), "payload": hex.EncodeToString(p.Payload),
			})
			fmt.Println(string(b))
			if n := puntCount.Add(1); puntLimit > 0 && n >= puntLimit {
				puntsOnce.Do(func() { close(puntsDone) })
			}
		},
	})
	if err != nil {
		die("%v", err)
	}

	switch cmd {
	case "up":
		if id > 255 || port == 0 || keyFile == "" {
			usage()
		}
		reply, err := dp.DeviceSet(dpproto.DeviceSet{
			LocalID: uint32(id), PrivateKey: readKey(keyFile), ListenPort: uint16(port),
			Fwmark: uint32(fwmark), MTU: uint32(mtu),
		})
		if err != nil {
			die("%v", err)
		}
		fmt.Println(base64.StdEncoding.EncodeToString(reply.PubKey[:]))
		dp.Detach()
		return
	case "down":
		if _, err := dp.Attach(); err != nil {
			die("%v", err)
		}
		dp.Close()
		return
	}

	if _, err := dp.Attach(); err != nil {
		die("%v", err)
	}
	defer dp.Detach()

	switch cmd {
	case "links":
		ls, err := dp.LinkList()
		if err != nil {
			die("%v", err)
		}
		for _, l := range ls {
			hs := "never"
			if l.LastHandshakeUnixNano != 0 {
				hs = time.Since(time.Unix(0, l.LastHandshakeUnixNano)).Round(time.Millisecond).String() + " ago"
			}
			fmt.Printf("%d %s endpoint=%s handshake=%s tx=%d rx=%d\n", l.PeerID,
				base64.StdEncoding.EncodeToString(l.PubKey[:]), l.Endpoint, hs, l.TxBytes, l.RxBytes)
		}
	case "link-set":
		if len(args) < 2 || len(args) > 3 {
			usage()
		}
		m := dpproto.LinkSet{PeerID: parseID(args[0]), PubKey: parseKey(args[1])}
		if len(args) == 3 {
			m.Endpoint = args[2]
		}
		if err := dp.LinkSet(m); err != nil {
			die("%v", err)
		}
	case "link-del":
		if len(args) != 1 {
			usage()
		}
		if err := dp.LinkDel(parseID(args[0])); err != nil {
			die("%v", err)
		}
	case "tuns":
		ts, err := dp.TunList()
		if err != nil {
			die("%v", err)
		}
		for _, t := range ts {
			fmt.Printf("%d %s ifindex=%d\n", t.PeerID, t.Name, t.IfIndex)
		}
	case "tun-set":
		if len(args) != 2 {
			usage()
		}
		t, err := dp.TunSet(dpproto.TunSet{PeerID: parseID(args[0]), Name: args[1]})
		if err != nil {
			die("%v", err)
		}
		fmt.Printf("%d %s ifindex=%d\n", t.PeerID, t.Name, t.IfIndex)
	case "tun-del":
		if len(args) != 1 {
			usage()
		}
		if err := dp.TunDel(parseID(args[0])); err != nil {
			die("%v", err)
		}
	case "routes":
		rs, err := dp.RouteList()
		if err != nil {
			die("%v", err)
		}
		for _, r := range rs {
			fmt.Printf("%d via %d\n", r.Dst, r.NextHop)
		}
	case "route-set":
		var rs []dpproto.Route
		for _, a := range args {
			d, n, ok := strings.Cut(a, ":")
			if !ok {
				die("route %q is not DST:NEXTHOP", a)
			}
			rs = append(rs, dpproto.Route{Dst: parseID(d), NextHop: parseID(n)})
		}
		if err := dp.RouteSet(rs); err != nil {
			die("%v", err)
		}
	case "inject":
		if len(args) != 5 {
			usage()
		}
		payload, err := hex.DecodeString(args[4])
		if err != nil {
			die("payload is not hex: %v", err)
		}
		if err := dp.Inject(dpproto.Inject{
			Link: parseID(args[0]), Proto: uint8(parseID(args[1])), Dst: uint8(parseID(args[2])),
			TTL: uint8(parseID(args[3])), Payload: payload,
		}); err != nil {
			die("%v", err)
		}
	case "stats":
		all := len(args) > 0 && args[0] == "-all"
		ss, err := dp.Stats()
		if err != nil {
			die("%v", err)
		}
		for _, s := range ss {
			if all || s.Value != 0 {
				fmt.Printf("%s %d\n", dpproto.StatName[s.ID], s.Value)
			}
		}
	case "punts":
		fs := flag.NewFlagSet("punts", flag.ExitOnError)
		n := fs.Int("n", 0, "stop after this many packets")
		t := fs.Float64("t", 0, "stop after this many seconds")
		_ = fs.Parse(args)
		puntLimit = int64(*n)
		if err := dp.StartPunts(); err != nil {
			die("%v", err)
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		var timeout <-chan time.Time
		if *t > 0 {
			timeout = time.After(time.Duration(*t * float64(time.Second)))
		}
		select {
		case <-sig:
		case <-puntsDone:
		case <-timeout:
		}
	default:
		usage()
	}
}
