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
	"os"
	"os/exec"
	"testing"
	"unsafe"
)

// The socket-option calls must be right at every stack depth, including the
// depths where the goroutine stack has to grow during the call and is moved.
//
// The test re-runs itself in a child process with GODEBUG=efence=1. In that
// mode the runtime unmaps a goroutine's old stack when it moves it, so a
// syscall that still holds a pointer into the old stack fails with EFAULT
// every time, instead of quietly reading or overwriting freed memory (which
// is what happens without efence, and only shows up now and then).
//
// Each call runs in a fresh goroutine (a small stack) at a different depth,
// so some calls land exactly where the stack has to grow. The deprecated
// uintptr Getsockopt is swept the same way as the aim check: if it never
// fails, these depths no longer make the stack move during the call and the
// sweep proves nothing.

const stackChildEnv = "SCTP_TEST_STACK_GROWTH_CHILD"

// atDepth calls f under n extra stack frames.
//
//go:noinline
func atDepth(n int, f func()) {
	var pad [64]byte
	if n == 0 {
		f()
		return
	}
	pad[n%len(pad)] = byte(n)
	atDepth(n-1, f)
	_ = pad
}

// sweep runs f once per depth, each time in a new goroutine, and returns how
// many runs reported a failure.
func sweep(depths int, f func() bool) int {
	failed := 0
	for d := 0; d < depths; d++ {
		done := make(chan bool)
		go func(d int) { atDepth(d, func() { done <- f() }) }(d)
		if !<-done {
			failed++
		}
	}
	return failed
}

// multiHomedConn dials a loopback association to a listener bound to
// 127.0.0.1 and 127.0.0.2, so the peer address list has two entries.
func multiHomedConn(t *testing.T) *SCTPConn {
	t.Helper()
	laddr, err := ResolveSCTPAddr("sctp", "127.0.0.1/127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ListenSCTP("sctp", laddr)
	if err != nil {
		t.Skipf("SCTP is not available here: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	raddr := &SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, Port: ln.Addr().(*SCTPAddr).Port}
	conn, err := DialSCTP("sctp", nil, raddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestSocketOptionsSurviveStackGrowth(t *testing.T) {
	if os.Getenv(stackChildEnv) == "" {
		godebug := "efence=1"
		if v := os.Getenv("GODEBUG"); v != "" {
			godebug = v + "," + godebug
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestSocketOptionsSurviveStackGrowth$", "-test.v")
		cmd.Env = append(os.Environ(), stackChildEnv+"=1", "GODEBUG="+godebug)
		out, err := cmd.CombinedOutput()
		t.Logf("child (GODEBUG=%s):\n%s", godebug, out)
		if err != nil {
			t.Fatalf("child failed: %v", err)
		}
		return
	}

	conn := multiHomedConn(t)
	const depths = 1600

	// getsockopt with a 4 KB stack buffer.
	remote := sweep(depths, func() bool {
		ra, ok := conn.RemoteAddr().(*SCTPAddr)
		return ok && ra != nil && len(ra.IPAddrs) == 2
	})
	local := sweep(depths, func() bool {
		la, ok := conn.LocalAddr().(*SCTPAddr)
		return ok && la != nil && len(la.IPAddrs) > 0 && la.Port != 0
	})
	// setsockopt, then getsockopt, with small stack structs.
	flip := 0
	events := sweep(depths, func() bool {
		want := SCTP_EVENT_DATA_IO
		if flip++; flip%2 == 0 {
			want |= SCTP_EVENT_ASSOCIATION
		}
		if err := conn.SubscribeEvents(want); err != nil {
			return false
		}
		got, err := conn.SubscribedEvents()
		return err == nil && got == want
	})
	// The aim: the deprecated uintptr Getsockopt, the pattern this test guards
	// against, must fail at some of these depths.
	canary := sweep(depths, func() bool {
		var param struct {
			assocID int32
			addrNum uint32
			addrs   [4096]byte
		}
		optlen := unsafe.Sizeof(param)
		_, _, err := conn.Getsockopt(SCTP_GET_PEER_ADDRS, uintptr(unsafe.Pointer(&param)), uintptr(unsafe.Pointer(&optlen)))
		return err == nil && param.addrNum == 2
	})

	t.Logf("failures over %d depths: RemoteAddr %d, LocalAddr %d, SubscribeEvents %d; deprecated uintptr Getsockopt (aim) %d",
		depths, remote, local, events, canary)
	if remote+local+events != 0 {
		t.Errorf("socket-option calls failed when the stack grew: RemoteAddr %d, LocalAddr %d, SubscribeEvents %d", remote, local, events)
	}
	if canary == 0 {
		t.Fatal("the uintptr Getsockopt never failed: these depths no longer make the stack grow during the call, so this test proves nothing - widen them")
	}
}
