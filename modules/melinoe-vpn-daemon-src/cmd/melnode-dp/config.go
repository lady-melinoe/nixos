package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the data plane's own (small, static) configuration: who it is and
// how it reaches the network. Everything dynamic -- which links exist, which
// tuns, which routes -- is programmed over the control socket by the control
// plane (melnode-cp) instead, exactly so this half can stay a dumb, fast
// forwarder.
type Config struct {
	LocalID   int    `toml:"localID"`
	LocalPort int    `toml:"localPort"`
	TunPrefix string `toml:"tunPrefix"`

	LocalPrivkey string `toml:"localPrivkey"`
	// LocalPrivkeyPath is an alternative to LocalPrivkey: a file holding
	// the base64 private key (surrounding whitespace ignored), so the
	// secret needn't live in the config itself. Exactly one of the two
	// must be set.
	LocalPrivkeyPath string `toml:"localPrivkeyPath"`

	// MTU of every tun. The default (defaultMTU, router.go) is WireGuard's
	// 1420 minus melnode's 4-byte routing header.
	MTU    int `toml:"mtu"`
	Fwmark int `toml:"fwmark"` // 0 (default) means unset -- see conn.Bind.SetMark; SO_MARK, Linux-only

	// Socket is the path of the unix seqpacket socket the control plane
	// attaches to (dpproto). Required: without it nothing could ever
	// program this data plane.
	Socket string `toml:"socket"`
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{
		TunPrefix: "node-",
		MTU:       defaultMTU,
	}
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

// privateKey returns the configured static private key, reading it from
// localPrivkeyPath if that is what was configured.
func (c *Config) privateKey() (NoisePrivateKey, error) {
	if c.LocalPrivkeyPath == "" {
		return parseKeyBase64(c.LocalPrivkey)
	}
	raw, err := os.ReadFile(c.LocalPrivkeyPath)
	if err != nil {
		return NoisePrivateKey{}, fmt.Errorf("reading localPrivkeyPath: %w", err)
	}
	return parseKeyBase64(strings.TrimSpace(string(raw)))
}

func (c *Config) validate() error {
	if c.LocalID < 0 || c.LocalID > 255 {
		return fmt.Errorf("localID must be 0-255, got %d", c.LocalID)
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
	if c.Fwmark < 0 || c.Fwmark > 0xffffffff {
		return fmt.Errorf("fwmark must fit in a uint32, got %d", c.Fwmark)
	}
	if c.Socket == "" {
		return fmt.Errorf("socket is required (the control plane attaches there)")
	}
	return nil
}
