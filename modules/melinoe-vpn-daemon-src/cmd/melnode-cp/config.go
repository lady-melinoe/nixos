package main

import (
	"encoding/base64"
	"fmt"

	"github.com/BurntSushi/toml"
)

type LinkConfig struct {
	PeerID       int    `toml:"peerid"`
	Endpoint     string `toml:"endpoint"` // "" means listen-only (no dial target)
	PeerPubkey   string `toml:"peerPubkey"`
	PrependCount int    `toml:"prependCount"` // AS-prepending for path-vector traffic engineering, see pathvector.go's forwardPath; 0 (default) is plain shortest-path
}

// Config is the control plane's configuration. The data plane has its own
// small config (identity key, UDP port, MTU); everything about *who to talk
// to* lives here and is pushed down to it on attach.
type Config struct {
	LocalID int `toml:"localID"`

	// DataplaneSocket is the data plane's control socket (its `socket`
	// setting): where this process attaches.
	DataplaneSocket string `toml:"dataplaneSocket"`

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
	// endpoints as the control socket, but never /advertise or /withdraw.
	// Unauthenticated -- restrict reachability with the host firewall.
	// "" (default) disables it.
	IntrospectListen string `toml:"introspectListen"`

	Links []LinkConfig `toml:"link"`
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{}
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

func (c *Config) validate() error {
	if c.LocalID < 0 || c.LocalID > 255 {
		return fmt.Errorf("localID must be 0-255, got %d", c.LocalID)
	}
	if c.DataplaneSocket == "" {
		return fmt.Errorf("dataplaneSocket is required")
	}
	if c.IdentityPrefix != "" {
		if _, ok := parsePrefix(c.IdentityPrefix); !ok {
			return fmt.Errorf("identityPrefix %q is not a valid IPv4 prefix (e.g. \"10.99.0.1/32\")", c.IdentityPrefix)
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
