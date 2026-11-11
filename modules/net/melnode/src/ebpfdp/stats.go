package ebpfdp

import (
	"strings"

	"melnode/dpproto"
)

func registerStatNames(names []string) {
	for i, n := range names {
		id := uint16(i + 1)
		if _, ok := dpproto.StatName[id]; !ok {
			dpproto.StatName[id] = strings.ToLower(n)
		}
	}
}
