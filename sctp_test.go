package sctp

import (
	"net"
	"reflect"
	"testing"
)

type resolveSCTPAddrTest struct {
	network       string
	litAddrOrName string
	addr          *SCTPAddr
	err           error
}

var resolveSCTPAddrTests = []resolveSCTPAddrTest{
	{"sctp", "127.0.0.1:0", &SCTPAddr{IP: []net.IP{net.IPv4(127, 0, 0, 1)}, Port: 0}, nil},
	{"sctp4", "127.0.0.1:65535", &SCTPAddr{IP: []net.IP{net.IPv4(127, 0, 0, 1)}, Port: 65535}, nil},

	{"sctp", "[::1]:0", &SCTPAddr{IP: []net.IP{net.ParseIP("::1")}, Port: 0}, nil},
	{"sctp6", "[::1]:65535", &SCTPAddr{IP: []net.IP{net.ParseIP("::1")}, Port: 65535}, nil},

	{"sctp", ":12345", &SCTPAddr{Port: 12345}, nil},

	{"sctp", "127.0.0.1/10.0.0.1:0", &SCTPAddr{IP: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv4(10, 0, 0, 1)}, Port: 0}, nil},
	{"sctp4", "127.0.0.1/10.0.0.1:65535", &SCTPAddr{IP: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv4(10, 0, 0, 1)}, Port: 65535}, nil},
}

func TestResolveSCTPAddr(t *testing.T) {
	for _, tt := range resolveSCTPAddrTests {
		addr, err := ResolveSCTPAddr(tt.network, tt.litAddrOrName)
		if !reflect.DeepEqual(addr, tt.addr) || !reflect.DeepEqual(err, tt.err) {
			t.Errorf("ResolveSCTPAddr(%q, %q) = %#v, %v, want %#v, %v", tt.network, tt.litAddrOrName, addr, err, tt.addr, tt.err)
			continue
		}
		if err == nil {
			addr2, err := ResolveSCTPAddr(addr.Network(), addr.String())
			if !reflect.DeepEqual(addr2, tt.addr) || err != tt.err {
				t.Errorf("(%q, %q): ResolveSCTPAddr(%q, %q) = %#v, %v, want %#v, %v", tt.network, tt.litAddrOrName, addr.Network(), addr.String(), addr2, err, tt.addr, tt.err)
			}
		}
	}
}
