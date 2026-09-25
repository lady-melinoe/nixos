package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

type LinkConfig struct {
	PeerID       int    `toml:"peerid"`
	Endpoint     string `toml:"endpoint"` // "" means listen-only (no dial target)
	PeerPubkey   string `toml:"peerPubkey"`
	PrependCount int    `toml:"prependCount"` // AS-prepending for path-vector traffic engineering, see pathvector.go's forwardPath; 0 (default) is plain shortest-path
}

// Config is the control plane's configuration -- all of it. The data plane
// has no config file: it is started with just a socket path and is configured
// entirely from here on attach (dpproto's DeviceSet for the device itself,
// then links, tuns and routes).
type Config struct {
	LocalID int `toml:"localID"`

	// DataplaneSocket is the data plane's control socket: where this process
	// attaches, and (when DataplaneCommand is set) what it starts the data
	// plane listening on. Ignored (and not required) when KernelDataplane is
	// set.
	DataplaneSocket string `toml:"dataplaneSocket"`

	// KernelDataplane, if true, attaches to the melnode kernel module
	// (modules/melinoe-vpn-kernel) over generic netlink instead of dialing
	// DataplaneSocket. Mutually exclusive with DataplaneCommand/
	// DataplaneSocket: a kernel data plane is not a process this one starts,
	// adopts, or asks to exit (dpproto.Client.Quit is a no-op for it).
	KernelDataplane bool `toml:"kernelDataplane"`

	// DataplaneCommand, if set, is the data plane's argv (e.g.
	// ["/path/to/melnode-dp"]); this process then owns getting it running:
	// on attach it adopts a data plane that is already there, or else starts
	// one (with `-socket DataplaneSocket` appended) detached from itself, so
	// the data plane keeps forwarding if this process restarts. An adopted
	// data plane is replaced if it isn't running the configured binary, or
	// was configured differently. Unset means the data plane is supervised
	// elsewhere (e.g. its own systemd unit): it is never started, only
	// attached to.
	DataplaneCommand []string `toml:"dataplaneCommand"`

	// The data plane's device settings, pushed to it on attach.
	LocalPort int `toml:"localPort"` // UDP port every node listens on and every link dials
	// LocalPrivkeyPath is a file holding this node's base64 Curve25519
	// private key (surrounding whitespace ignored); LocalPrivkey is the same
	// key inline. Exactly one must be set. The key is sent to the data plane
	// over its socket, never written anywhere.
	LocalPrivkeyPath string `toml:"localPrivkeyPath"`
	LocalPrivkey     string `toml:"localPrivkey"`
	MTU              int    `toml:"mtu"`    // of every tun; the default is WireGuard's 1420 minus melnode's 4-byte routing header
	Fwmark           int    `toml:"fwmark"` // SO_MARK on the data plane's UDP socket; 0 (default) means unset

	// TunPrefix names the tuns: "<tunPrefix><peerid>" (e.g. "node-4").
	TunPrefix string `toml:"tunPrefix"`

	// TunCreateHookBin / TunDestroyHookBin, if set, are executables run as
	// `<bin> <peerid> <ifname>` after a peer tun is created and brought
	// up, and after it is deleted. "" disables. Hook failures are logged
	// but never fatal.
	TunCreateHookBin  string `toml:"tunCreateHookBin"`
	TunDestroyHookBin string `toml:"tunDestroyHookBin"`

	// IdentityPrefix is this node's own always-advertised prefix (a
	// single /32 identity address, e.g. the node's own loopback --
	// mirrors "melinoe"'s FRR `network <loopback>/32` statement), also
	// assigned as the address of every tun. "" == none. Everything else
	// this node advertises comes in dynamically via ControlSocket, from an
	// external process.
	IdentityPrefix string `toml:"identityPrefix"`

	// ControlSocket, if set, is a filesystem path where the local HTTP
	// control API (controlapi.go) listens, used to advertise/withdraw
	// prefixes at runtime. "" (default) disables it entirely.
	ControlSocket string `toml:"controlSocket"`

	// IntrospectListen, if set, is a TCP listen address (e.g. ":60198")
	// for a READ-ONLY HTTP API (introspectapi.go): the same "show ..."
	// endpoints as the control socket, but never /advertise or /withdraw,
	// and served from a snapshot rebuilt every IntrospectIntervalMs.
	// Unauthenticated -- restrict reachability with the host firewall.
	// "" (default) disables it.
	IntrospectListen string `toml:"introspectListen"`
	// IntrospectIntervalMs is how often the TCP API's snapshot is rebuilt
	// (introspectapi.go): the most out of date its answers can be, and
	// how often it reads the data plane whether or not anyone asks.
	IntrospectIntervalMs int `toml:"introspectIntervalMs"`

	Links []LinkConfig `toml:"link"`
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{TunPrefix: "node-", MTU: defaultMTU, IntrospectIntervalMs: 1000}
	meta, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		return nil, fmt.Errorf("unknown config key(s): %v", undecoded)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func parsePubKeyBase64(s string) ([32]byte, error) {
	var key [32]byte
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return key, fmt.Errorf("invalid base64 key: %w", err)
	}
	if len(decoded) != len(key) {
		return key, fmt.Errorf("key must decode to exactly %d bytes, got %d", len(key), len(decoded))
	}
	copy(key[:], decoded)
	return key, nil
}

// privateKey returns the configured static private key.
func (c *Config) privateKey() ([32]byte, error) {
	if c.LocalPrivkeyPath == "" {
		return parsePubKeyBase64(c.LocalPrivkey)
	}
	raw, err := os.ReadFile(c.LocalPrivkeyPath)
	if err != nil {
		return [32]byte{}, fmt.Errorf("reading localPrivkeyPath: %w", err)
	}
	return parsePubKeyBase64(strings.TrimSpace(string(raw)))
}

func (c *Config) validate() error {
	if c.LocalID < 0 || c.LocalID > 255 {
		return fmt.Errorf("localID must be 0-255, got %d", c.LocalID)
	}
	if c.KernelDataplane {
		if c.DataplaneSocket != "" {
			return fmt.Errorf("dataplaneSocket and kernelDataplane are mutually exclusive")
		}
		if len(c.DataplaneCommand) > 0 {
			return fmt.Errorf("dataplaneCommand and kernelDataplane are mutually exclusive")
		}
	} else if c.DataplaneSocket == "" {
		return fmt.Errorf("dataplaneSocket is required (unless kernelDataplane is set)")
	}
	if c.LocalPort <= 0 || c.LocalPort > 65535 {
		return fmt.Errorf("localPort must be a valid port, got %d", c.LocalPort)
	}
	if c.LocalPrivkey == "" && c.LocalPrivkeyPath == "" {
		return fmt.Errorf("one of localPrivkey or localPrivkeyPath is required")
	}
	if c.LocalPrivkey != "" && c.LocalPrivkeyPath != "" {
		return fmt.Errorf("localPrivkey and localPrivkeyPath are mutually exclusive")
	}
	if c.MTU < 576 || c.MTU > 65000 {
		return fmt.Errorf("mtu must be between 576 and 65000, got %d", c.MTU)
	}
	if c.IntrospectIntervalMs < 100 || c.IntrospectIntervalMs > 60000 {
		return fmt.Errorf("introspectIntervalMs must be between 100 and 60000, got %d", c.IntrospectIntervalMs)
	}
	if c.Fwmark < 0 || c.Fwmark > 0xffffffff {
		return fmt.Errorf("fwmark must fit in a uint32, got %d", c.Fwmark)
	}
	if c.TunPrefix == "" || len(c.TunPrefix) > 12 || strings.ContainsAny(c.TunPrefix, "/ \t\n") {
		return fmt.Errorf("tunPrefix %q must be 1-12 characters with no spaces or slashes (interface names are at most 15, and the peerid follows)", c.TunPrefix)
	}
	if len(c.DataplaneCommand) > 0 && c.DataplaneCommand[0] == "" {
		return fmt.Errorf("dataplaneCommand[0] is empty")
	}
	if c.IdentityPrefix != "" {
		if _, ok := parsePrefix(c.IdentityPrefix); !ok {
			return fmt.Errorf("identityPrefix %q is not a valid IPv4 prefix without host bits (e.g. \"10.99.0.1/32\")", c.IdentityPrefix)
		}
	}
	seenLinks := map[int]bool{}
	for _, l := range c.Links {
		if l.PeerID < 0 || l.PeerID > 255 {
			return fmt.Errorf("[[link]] peerid must be 0-255, got %d", l.PeerID)
		}
		if l.PeerID == c.LocalID {
			return fmt.Errorf("[[link]] peerid %d is this node itself", l.PeerID)
		}
		if seenLinks[l.PeerID] {
			return fmt.Errorf("duplicate [[link]] peerid %d", l.PeerID)
		}
		seenLinks[l.PeerID] = true
		if l.PeerPubkey == "" {
			return fmt.Errorf("[[link]] peerid %d missing peerPubkey", l.PeerID)
		}
		if _, err := parsePubKeyBase64(l.PeerPubkey); err != nil {
			return fmt.Errorf("[[link]] peerid %d: invalid peerPubkey: %w", l.PeerID, err)
		}
		if l.PrependCount < 0 {
			return fmt.Errorf("[[link]] peerid %d prependCount must be >= 0, got %d", l.PeerID, l.PrependCount)
		}
		if l.PrependCount > pvMaxPathLen {
			// Path lengths are a single byte on the wire (pathvector.go).
			return fmt.Errorf("[[link]] peerid %d prependCount must be <= %d, got %d", l.PeerID, pvMaxPathLen, l.PrependCount)
		}
	}
	return nil
}
