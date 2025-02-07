//go:build linux && !386
// +build linux,!386

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
	"net"
	"syscall"
	"testing"
)

func TestUseControlFuncWithoutLocalAddress(t *testing.T) {
	network := "sctp"
	raddr := &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}}}
	initMsg := InitMsg{
		NumOstreams:    0,
		MaxInstreams:   0,
		MaxAttempts:    0,
		MaxInitTimeout: 0,
	}
	customControlFunc := func(networkFunc, address string, c syscall.RawConn) error {
		if network != networkFunc {
			t.Fatalf("network invalid: %s", networkFunc)
		}
		if address != "" {
			t.Fatalf("address not empty: %s", address)
		}
		if c == nil {
			t.Fatal("RawConn is nil")
		}
		return nil
	}
	conn, err := dialSCTPExtConfig(network, nil, raddr, initMsg, customControlFunc)
	if err != nil {
		t.Fatalf("failed to dial SCTP connection due to: %v", err)
	}
	conn.Close()
}
