package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(b byte) string {
	var k [32]byte
	for i := range k {
		k[i] = b
	}
	return base64.StdEncoding.EncodeToString(k[:])
}

func validTestConfig() *Config {
	return &Config{
		LocalID:              1,
		EbpfObject:           "melnode.bpf.o",
		LocalPort:            60198,
		LocalPrivkey:         testKey(9),
		MTU:                  defaultMTU,
		IntrospectIntervalMs: 1000,
		TunPrefix:            "node-",
		Links:                []LinkConfig{{PeerID: 2, PeerPubkey: testKey(2), Endpoint: "192.0.2.2:60198"}},
	}
}

func TestValidateLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string // a substring of the error, or "" for none
	}{
		{"valid", func(*Config) {}, ""},
		{"listen-only link", func(c *Config) { c.Links[0].Endpoint = "" }, ""},
		{"ipv6 endpoint", func(c *Config) { c.Links[0].Endpoint = "[2001:db8::2]:60198" }, ""},
		{"wireguard style endpoint in brackets", func(c *Config) { c.Links[0].Endpoint = "[192.0.2.2:51820]:60198" }, "not an ip:port literal"},
		{"hostname endpoint", func(c *Config) { c.Links[0].Endpoint = "peer.example.org:60198" }, "not an ip:port literal"},
		{"endpoint without port", func(c *Config) { c.Links[0].Endpoint = "192.0.2.2" }, "not an ip:port literal"},
		{"endpoint with a zone", func(c *Config) { c.Links[0].Endpoint = "[fe80::2%eth0]:60198" }, "zone"},
		{"endpoint with port 0", func(c *Config) { c.Links[0].Endpoint = "192.0.2.2:0" }, "port 0"},
		{"all-zero peer key", func(c *Config) { c.Links[0].PeerPubkey = base64.StdEncoding.EncodeToString(make([]byte, 32)) }, "all zeros"},
		{"two links with one key", func(c *Config) {
			c.Links = append(c.Links, LinkConfig{PeerID: 3, PeerPubkey: c.Links[0].PeerPubkey})
		}, "same peerPubkey"},
		{"prependCount at the largest usable value", func(c *Config) { c.Links[0].PrependCount = pvMaxPathLen - 1 }, ""},
		{"prependCount that leaves no room for the origin", func(c *Config) { c.Links[0].PrependCount = pvMaxPathLen }, "prependCount must be <="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validTestConfig()
			tc.edit(c)
			err := c.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("validate() = nil, want an error containing %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("validate() = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
