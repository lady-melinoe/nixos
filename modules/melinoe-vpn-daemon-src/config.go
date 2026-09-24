package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

type LinkConfig struct {
	PeerID       int    `toml:"peerid"`
	Endpoint     string `toml:"endpoint"` // "" means listen-only (no dial target)
	PeerPubkey   string `toml:"peerPubkey"`
	PrependCount int    `toml:"prependCount"` // AS-prepending for path-vector traffic engineering, see pathvector.go's forwardPath; 0 (default) is plain shortest-path, no config change in behavior
}

type Config struct {
	LocalID      int    `toml:"localID"`
	LocalPort    int    `toml:"localPort"`
	TunPrefix    string `toml:"tunPrefix"`
	LocalPrivkey string `toml:"localPrivkey"`
	// LocalPrivkeyPath is an alternative to LocalPrivkey: a file holding
	// the base64 private key (surrounding whitespace ignored), so the
	// secret needn't live in the config itself. Exactly one of the two
	// must be set.
	LocalPrivkeyPath string `toml:"localPrivkeyPath"`

	// TunCreateHookBin / TunDestroyHookBin, if set, are executables run as
	// `<bin> <peerid> <ifname>` after a peer tun is created and brought
	// up, and after it is closed/deleted (incl. at shutdown). "" disables.
	// Hook failures are logged but never fatal.
	TunCreateHookBin  string `toml:"tunCreateHookBin"`
	TunDestroyHookBin string `toml:"tunDestroyHookBin"`
	Fwmark            int    `toml:"fwmark"` // 0 (default) means unset -- see conn.Bind.SetMark; SO_MARK, Linux-only

	// IdentityPrefix is this node's own always-advertised prefix (a
	// single /32 identity address, e.g. the node's own loopback --
	// mirrors "melinoe"'s FRR `network <loopback>/32` statement). "" ==
	// none. Everything else this node advertises comes in dynamically
	// via ControlSocket, from an external process -- melnode itself
	// never scans for or discovers routes beyond this one static entry
	// and its own peerid. See PROJECT_STATE.md.
	IdentityPrefix string `toml:"identityPrefix"`

	// ControlSocket, if set, is a filesystem path where melnode listens
	// for the local HTTP control API (controlapi.go) used to
	// advertise/withdraw prefixes at runtime. "" (default) disables it
	// entirely -- no local control surface, only the static
	// IdentityPrefix (if any) and the mesh-discovered peerid routes.
	ControlSocket string `toml:"controlSocket"`

	Links []LinkConfig `toml:"link"`
}

func loadConfig(path string) (*Config, error) {
	cfg := &Config{
		TunPrefix: "node-",
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
	if c.Fwmark < 0 || c.Fwmark > 0xffffffff {
		return fmt.Errorf("fwmark must fit in a uint32, got %d", c.Fwmark)
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
		if seenLinks[l.PeerID] {
			return fmt.Errorf("duplicate [[link]] peerid %d", l.PeerID)
		}
		seenLinks[l.PeerID] = true
		if l.PeerPubkey == "" {
			return fmt.Errorf("[[link]] peerid %d missing peerPubkey", l.PeerID)
		}
		if l.PrependCount < 0 {
			return fmt.Errorf("[[link]] peerid %d prependCount must be >= 0, got %d", l.PeerID, l.PrependCount)
		}
	}
	// [[peer]]/nhid used to be validated here too, back when reachability
	// was static config. It's now discovered at runtime by the
	// path-vector protocol (pathvector.go) instead -- see
	// PROJECT_STATE.md.
	return nil
}
