// Copyright 2019 Wataru Ishida. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sctp

import (
	"bytes"
	"io"
	"net"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"testing"
)

type resolveSCTPAddrTest struct {
	network       string
	litAddrOrName string
	addr          *SCTPAddr
	err           error
}

var resolveSCTPAddrTests = []resolveSCTPAddrTest{
	{"sctp", "127.0.0.1:0", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}}, Port: 0}, nil},
	{"sctp4", "127.0.0.1:65535", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}}, Port: 65535}, nil},

	{"sctp", "[::1]:0", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("::1")}}, Port: 0}, nil},
	{"sctp6", "[::1]:65535", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("::1")}}, Port: 65535}, nil},

	{"sctp", "[fe80::1%eth0]:0", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"}}, Port: 0}, nil},
	{"sctp6", "[fe80::1%eth0]:65535", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"}}, Port: 65535}, nil},

	{"sctp", ":12345", &SCTPAddr{Port: 12345}, nil},

	{"sctp", "127.0.0.1/10.0.0.1:0", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}, net.IPAddr{IP: net.IPv4(10, 0, 0, 1)}}, Port: 0}, nil},
	{"sctp4", "127.0.0.1/10.0.0.1:65535", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}, net.IPAddr{IP: net.IPv4(10, 0, 0, 1)}}, Port: 65535}, nil},
}

func TestSCTPAddrString(t *testing.T) {
	for _, tt := range resolveSCTPAddrTests {
		s := tt.addr.String()
		if tt.litAddrOrName != s {
			t.Errorf("expected %q, got %q", tt.litAddrOrName, s)
		}
	}
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

var sctpListenerNameTests = []struct {
	net   string
	laddr *SCTPAddr
}{
	{"sctp4", &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}}}},
	{"sctp4", &SCTPAddr{}},
	{"sctp4", nil},
	{"sctp", &SCTPAddr{Port: 7777}},
}

func TestSCTPListenerName(t *testing.T) {
	for _, tt := range sctpListenerNameTests {
		ln, err := ListenSCTP(tt.net, tt.laddr)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		la := ln.Addr()
		if a, ok := la.(*SCTPAddr); !ok || a.Port == 0 {
			t.Fatalf("got %v; expected a proper address with non-zero port number", la)
		}
	}
}

func TestSCTPConcurrentAccept(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	addr, _ := ResolveSCTPAddr("sctp", "127.0.0.1:0")
	ln, err := ListenSCTP("sctp", addr)
	if err != nil {
		t.Fatal(err)
	}
	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					break
				}
				c.Close()
			}
			wg.Done()
		}()
	}
	attempts := 10 * N
	fails := 0
	for i := 0; i < attempts; i++ {
		c, err := DialSCTP("sctp", nil, ln.Addr().(*SCTPAddr))
		if err != nil {
			fails++
		} else {
			c.Close()
		}
	}
	ln.Close()
	// BUG Accept() doesn't return even if we closed ln
	//	wg.Wait()
	if fails > 0 {
		t.Fatalf("# of failed Dials: %v", fails)
	}
}

func TestSCTPCloseRecv(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(4))
	addr, _ := ResolveSCTPAddr("sctp", "127.0.0.1:0")
	ln, err := ListenSCTP("sctp", addr)
	if err != nil {
		t.Fatal(err)
	}
	var conn net.Conn
	var wg sync.WaitGroup
	connReady := make(chan struct{}, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var xerr error
		conn, xerr = ln.Accept()
		if xerr != nil {
			t.Fatal(xerr)
		}
		connReady <- struct{}{}
		buf := make([]byte, 256)
		_, xerr = conn.Read(buf)
		t.Logf("got error while read: %v", xerr)
		if xerr != io.EOF && xerr != syscall.EBADF {
			t.Fatalf("read failed: %v", xerr)
		}
	}()

	_, err = DialSCTP("sctp", nil, ln.Addr().(*SCTPAddr))
	if err != nil {
		t.Fatalf("failed to dial: %s", err)
	}

	<-connReady
	err = conn.Close()
	if err != nil {
		t.Fatalf("close failed: %v", err)
	}
	wg.Wait()
}

func TestToNetworkByteOrderBuf(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		expected []byte
	}{
		{
			name:     "uint16",
			input:    uint16(0x1234),
			expected: []byte{0x12, 0x34},
		},
		{
			name:     "uint32",
			input:    uint32(0x12345678),
			expected: []byte{0x12, 0x34, 0x56, 0x78},
		},
		{
			name:     "uint64",
			input:    uint64(0x123456789ABCDEF0),
			expected: []byte{0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, 0xDE, 0xF0},
		},
		{
			name:     "int16",
			input:    int16(0x1234),
			expected: []byte{0x12, 0x34},
		},
		{
			name:     "int32",
			input:    int32(0x12345678),
			expected: []byte{0x12, 0x34, 0x56, 0x78},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toNetworkByteOrderBuf(tt.input)
			if !bytes.Equal(result, tt.expected) {
				t.Errorf("toNetworkByteOrderBuf(%v) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestFromNetworkByteOrderBuf(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		output   interface{}
		expected interface{}
		wantErr  bool
	}{
		{
			name:     "uint16",
			input:    []byte{0x12, 0x34},
			output:   new(uint16),
			expected: uint16(0x1234),
			wantErr:  false,
		},
		{
			name:     "uint32",
			input:    []byte{0x12, 0x34, 0x56, 0x78},
			output:   new(uint32),
			expected: uint32(0x12345678),
			wantErr:  false,
		},
		{
			name:     "uint64",
			input:    []byte{0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, 0xDE, 0xF0},
			output:   new(uint64),
			expected: uint64(0x123456789ABCDEF0),
			wantErr:  false,
		},
		{
			name:     "int16",
			input:    []byte{0x12, 0x34},
			output:   new(int16),
			expected: int16(0x1234),
			wantErr:  false,
		},
		{
			name:     "int32",
			input:    []byte{0x12, 0x34, 0x56, 0x78},
			output:   new(int32),
			expected: int32(0x12345678),
			wantErr:  false,
		},
		{
			name:     "invalid input length",
			input:    []byte{0x12},
			output:   new(uint16),
			expected: uint16(0),
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fromNetworkByteOrderBuf(tt.output, tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("fromNetworkByteOrderBuf() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !reflect.DeepEqual(reflect.ValueOf(tt.output).Elem().Interface(), tt.expected) {
				t.Errorf("fromNetworkByteOrderBuf() = %v, want %v", reflect.ValueOf(tt.output).Elem().Interface(), tt.expected)
			}
		})
	}
}
