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
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
)

func TestNotificationHandlerAssignmentOnDialing(t *testing.T) {
	network := "sctp"
	addr := &SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, Port: 54321}
	testErr := errors.New("test error")
	notificationHandler := func([]byte) error { return testErr }

	listener, err := ListenSCTP(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialSCTPExtConfig(network, nil, addr, InitMsg{}, nil, notificationHandler)
	if err != nil {
		t.Fatalf("failed to establish connection due to: %v", err)
	}
	if conn == nil || conn.notificationHandler(nil) != testErr {
		t.Fatalf("notification handler has not been assigned")
	}
	listener.Close()
	conn.Close()
}

func TestNotificationHandlerAssignmentOnListening(t *testing.T) {
	network := "sctp"
	addr := &SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, Port: 54321}
	testErr := errors.New("test error")
	notificationHandler := func([]byte) error { return testErr }

	listener, err := listenSCTPExtConfig(network, addr, InitMsg{}, nil, notificationHandler)
	if err != nil {
		t.Fatalf("failed to start listening due to: %v", err)
	}
	if listener == nil || listener.notificationHandler(nil) != testErr {
		t.Fatalf("notification handler has not been assigned")
	}
	listener.Close()
}

func TestDialUseControlFuncWithoutLocalAddress(t *testing.T) {
	network := "sctp"
	raddr := &SCTPAddr{IPAddrs: []net.IPAddr{net.IPAddr{IP: net.IPv4(127, 0, 0, 1)}}}
	initMsg := InitMsg{}
	customControlFunc := validationControlFunc(t, network)
	conn, err := dialSCTPExtConfig(network, nil, raddr, initMsg, customControlFunc, nil)
	if err != nil && !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("failed to dial connection due to: %v", err)
	}
	conn.Close()
}

func TestListenUseControlFuncWithoutLocalAddress(t *testing.T) {
	network := "sctp"
	initMsg := InitMsg{}
	customControlFunc := validationControlFunc(t, network)
	listener, err := listenSCTPExtConfig(network, nil, initMsg, customControlFunc, nil)
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer listener.Close()
}

func validationControlFunc(t *testing.T, network string) func(networkFunc, address string, c syscall.RawConn) error {
	return func(networkFunc, address string, c syscall.RawConn) error {
		if networkFunc != network {
			t.Errorf("unexpected network: got %s, want %s", networkFunc, network)
		}
		if address != "" {
			t.Error("expected empty address")
		}
		if c == nil {
			t.Error("RawConn is nil")
		}
		return nil
	}
}
