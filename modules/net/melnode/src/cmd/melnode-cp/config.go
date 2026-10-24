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
	Endpoint     string `toml:"endpoint"`
	PeerPubkey   string `toml:"peerPubkey"`
	PrependCount int    `toml:"prependCount"`
}

type Config struct {
	LocalID int `toml:"localID"`

	// EbpfObject is the melnode eBPF object (melnode.bpf.o). PinDir is where the data plane's
	// state is pinned (default /sys/fs/bpf/melnode); HubInterface names the device that owns
	// the UDP sockets (default <tunPrefix>hub). TimeScale divides the protocol timers (tests).
	EbpfObject   string `toml:"ebpfObject"`
	PinDir       string `toml:"pinDir"`
	HubInterface string `toml:"hubInterface"`
	TimeScale    int    `toml:"timeScale"`
	// EbpfVariables sets named tunables of the data plane object (its `const volatile` globals).
	EbpfVariables map[string]uint64 `toml:"ebpfVariables"`

	LocalPort        int    `toml:"localPort"`
	LocalPrivkeyPath string `toml:"localPrivkeyPath"`
	LocalPrivkey     string `toml:"localPrivkey"`
	MTU              int    `toml:"mtu"`
	Fwmark           int    `toml:"fwmark"`

	TunPrefix string `toml:"tunPrefix"`

	TunCreateHookBin  string `toml:"tunCreateHookBin"`
	TunDestroyHookBin string `toml:"tunDestroyHookBin"`

	IdentityPrefix string `toml:"identityPrefix"`

	ControlSocket string `toml:"controlSocket"`

	IntrospectListen     string `toml:"introspectListen"`
	IntrospectIntervalMs int    `toml:"introspectIntervalMs"`

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
	if c.EbpfObject == "" {
		return fmt.Errorf("ebpfObject is required (path to melnode.bpf.o)")
	}
	if c.TimeScale < 0 {
		return fmt.Errorf("timeScale must be >= 0, got %d", c.TimeScale)
	}
	if len(c.HubInterface) > 15 {
		return fmt.Errorf("hubInterface %q is longer than 15 characters", c.HubInterface)
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
			return fmt.Errorf("[[link]] peerid %d prependCount must be <= %d, got %d", l.PeerID, pvMaxPathLen, l.PrependCount)
		}
	}
	return nil
}
