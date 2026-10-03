package net_test

import (
	"testing"

	"github.com/xtls/xray-core/common/net"
)

func TestRawNetAddrNilAddress(t *testing.T) {
	for _, network := range []net.Network{net.Network_TCP, net.Network_UDP, net.Network_UNIX} {
		if addr := (net.Destination{Network: network}).RawNetAddr(); addr != nil {
			t.Fatalf("nil address for %v returned %v", network, addr)
		}
	}
}
