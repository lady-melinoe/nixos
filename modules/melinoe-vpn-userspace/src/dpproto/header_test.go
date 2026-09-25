package dpproto

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The C header is the specification a kernel module is written from; this
// test keeps it and the Go implementation from drifting apart. Every constant
// in the header must exist here with the same value, and vice versa.
func TestHeaderMatchesGo(t *testing.T) {
	src, err := os.ReadFile("melnode_genl.h")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint32{
		"MELNODE_GENL_VERSION": Version,

		"MELNODE_CMD_UNSPEC": 0, "MELNODE_CMD_HELLO": uint32(CmdHello), "MELNODE_CMD_ATTACH": uint32(CmdAttach),
		"MELNODE_CMD_DEVICE_SET": uint32(CmdDeviceSet), "MELNODE_CMD_DEVICE_DEL": uint32(CmdDeviceDel),
		"MELNODE_CMD_STATS_GET": uint32(CmdStatsGet), "MELNODE_CMD_LINK_ADD": uint32(CmdLinkAdd),
		"MELNODE_CMD_LINK_DEL": uint32(CmdLinkDel), "MELNODE_CMD_LINK_GET": uint32(CmdLinkGet),
		"MELNODE_CMD_TUN_CREATE": uint32(CmdTunCreate), "MELNODE_CMD_TUN_START": uint32(CmdTunStart),
		"MELNODE_CMD_TUN_DESTROY": uint32(CmdTunDestroy), "MELNODE_CMD_TUN_GET": uint32(CmdTunGet),
		"MELNODE_CMD_ROUTE_SET": uint32(CmdRouteSet), "MELNODE_CMD_ROUTE_DEL": uint32(CmdRouteDel),
		"MELNODE_CMD_ROUTE_GET": uint32(CmdRouteGet), "MELNODE_CMD_PUNT": uint32(CmdPunt),
		"MELNODE_CMD_INJECT": uint32(CmdInject), "MELNODE_CMD_EVENT": uint32(CmdEvent), "MELNODE_CMD_X_QUIT": uint32(CmdXQuit),

		"MELNODE_A_UNSPEC": uint32(AttrUnspec), "MELNODE_A_PAD": uint32(AttrPad),
		"MELNODE_A_API_VERSION": uint32(AttrAPIVersion), "MELNODE_A_PID": uint32(AttrPID),
		"MELNODE_A_CONFIGURED": uint32(AttrConfigured), "MELNODE_A_LOCAL_ID": uint32(AttrLocalID),
		"MELNODE_A_PRIVATE_KEY": uint32(AttrPrivateKey), "MELNODE_A_PUBLIC_KEY": uint32(AttrPublicKey),
		"MELNODE_A_LISTEN_PORT": uint32(AttrListenPort), "MELNODE_A_FWMARK": uint32(AttrFwmark),
		"MELNODE_A_MTU": uint32(AttrMTU), "MELNODE_A_PEER_ID": uint32(AttrPeerID),
		"MELNODE_A_ENDPOINT": uint32(AttrEndpoint), "MELNODE_A_LAST_HANDSHAKE": uint32(AttrLastHS),
		"MELNODE_A_TX_BYTES": uint32(AttrTxBytes), "MELNODE_A_RX_BYTES": uint32(AttrRxBytes),
		"MELNODE_A_TUN_NAME": uint32(AttrTunName), "MELNODE_A_TUN_STARTED": uint32(AttrTunStarted),
		"MELNODE_A_ROUTE_DST": uint32(AttrRouteDst), "MELNODE_A_ROUTE_NEXTHOP": uint32(AttrRouteNextHop),
		"MELNODE_A_STAT_ID": uint32(AttrStatID), "MELNODE_A_STAT_VALUE": uint32(AttrStatValue),
		"MELNODE_A_PKT_LINK": uint32(AttrPktLink), "MELNODE_A_PKT_PROTO": uint32(AttrPktProto),
		"MELNODE_A_PKT_SRC": uint32(AttrPktSrc), "MELNODE_A_PKT_DST": uint32(AttrPktDst),
		"MELNODE_A_PKT_TTL": uint32(AttrPktTTL), "MELNODE_A_PKT_DATA": uint32(AttrPktData),
		"MELNODE_A_EVT_KIND": uint32(AttrEvtKind), "MELNODE_A_EVT_TIME": uint32(AttrEvtTime),

		"MELNODE_EVENT_LINK_HANDSHAKE": uint32(EventLinkHandshake),

		"MELNODE_STAT_PUNT_SENT": uint32(StatPuntSent), "MELNODE_STAT_PUNT_DROPPED": uint32(StatPuntDropped),
		"MELNODE_STAT_INJECT_SENT": uint32(StatInjectSent), "MELNODE_STAT_INJECT_DROPPED": uint32(StatInjectDropped),
		"MELNODE_STAT_RX_NO_ROUTE": uint32(StatRxNoRoute), "MELNODE_STAT_RX_TTL_EXPIRED": uint32(StatRxTTLExpired),
		"MELNODE_STAT_RX_NO_TUN": uint32(StatRxNoTun), "MELNODE_STAT_RX_TUN_FULL": uint32(StatRxTunFull),
		"MELNODE_STAT_RX_BAD_PACKET": uint32(StatRxBadPacket), "MELNODE_STAT_TX_NO_ROUTE": uint32(StatTxNoRoute),
		"MELNODE_STAT_TX_QUEUE_FULL": uint32(StatTxQueueFull), "MELNODE_STAT_EVENTS_DROPPED": uint32(StatEventsDropped),
		"MELNODE_STAT_RX_QUEUE_FULL": uint32(StatRxQueueFull),
	}

	re := regexp.MustCompile(`(?m)^(?:#define\s+)?\s*(MELNODE_[A-Z0-9_]+)(?:\s*=\s*|\s+)(\d+)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		name := m[1]
		v, _ := strconv.ParseUint(m[2], 10, 32)
		w, ok := want[name]
		if !ok {
			t.Errorf("%s is in the header but not covered by this test/Go", name)
			continue
		}
		if uint32(v) != w {
			t.Errorf("%s = %d in the header, %d in Go", name, v, w)
		}
		seen[name] = true
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("%s is in Go but missing from the header", name)
		}
	}
	// Family name and message type namespace.
	if !regexp.MustCompile(`MELNODE_GENL_NAME\s+"` + FamilyName + `"`).Match(src) {
		t.Error("family name differs between header and Go")
	}
}
