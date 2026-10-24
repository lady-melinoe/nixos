package ebpfdp

import (
	"strings"

	"melnode/dpproto"
)

// statNames are the object's counters in the order of its MN_STATS list; the first twelve are the
// ones the old kernel module reported, with the same ids.
var statNames = []string{
	"PUNT_SENT", "PUNT_DROPPED", "INJECT_SENT", "INJECT_DROPPED", "RX_NO_ROUTE", "RX_TTL_EXPIRED",
	"RX_NO_TUN", "RX_TUN_FULL", "RX_BAD_PACKET", "TX_NO_ROUTE", "TX_QUEUE_FULL", "RX_QUEUE_FULL",
	"TX_OK", "TX_NO_TUN", "TX_NO_SESSION", "TX_NO_ENDPOINT", "TX_RESIZE_FAIL", "TX_CRYPT_FAIL",
	"TX_COUNTER_EXHAUSTED", "TX_SEND_FAIL", "FWD_OK", "RX_DATA_OK", "RX_KEEPALIVE", "RX_SHORT",
	"RX_BAD_INDEX", "RX_KEYPAIR_EXPIRED", "RX_PULL_FAIL", "RX_AUTH_FAIL", "RX_REPLAY",
	"RX_TRIM_FAIL", "RX_INIT", "RX_INIT_BAD_MAC1", "RX_INIT_AUTH_FAIL", "RX_INIT_NO_PEER",
	"RX_INIT_REPLAY", "RX_RESP", "RX_RESP_BAD", "RX_UNKNOWN_TYPE", "HS_INIT_SENT", "HS_RESP_SENT",
	"HS_COMPLETED", "EP_UPDATES", "EP_READ_FAIL", "UNDER_LOAD", "HS_COOKIE_SENT", "RX_COOKIE_OK",
	"RX_COOKIE_BAD", "RX_HS_RATELIMITED", "KEYS_WIPED",
}

func init() {
	// The twelve the old module had keep their names; the rest are new.
	for i, n := range statNames {
		id := uint16(i + 1)
		if _, ok := dpproto.StatName[id]; !ok {
			dpproto.StatName[id] = strings.ToLower(n)
		}
	}
}

// registerStatNames gives every counter of the object a name in dpproto.StatName (ids are 1-based).
func registerStatNames(names []string) {
	for i, n := range names {
		id := uint16(i + 1)
		if _, ok := dpproto.StatName[id]; !ok {
			dpproto.StatName[id] = strings.ToLower(n)
		}
	}
}
