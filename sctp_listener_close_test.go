//go:build linux && !386
// +build linux,!386

// Copyright 2024 Wataru Ishida. All rights reserved.
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
	"sync"
	"syscall"
	"testing"
	"time"
)

// newLoopbackListener returns a fresh SCTPListener on an ephemeral 127.0.0.1 port.
func newLoopbackListener(t *testing.T) *SCTPListener {
	t.Helper()
	addr := &SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}}
	ln, err := ListenSCTP("sctp", addr)
	if err != nil {
		t.Fatalf("ListenSCTP failed: %v", err)
	}
	return ln
}

// TestListenerCloseUnblocksAccept verifies that closing a listener while
// AcceptSCTP is blocked unblocks Accept and returns ErrListenerClosed (not a
// bare syscall.EBADF / syscall.EINVAL leaked from accept4).
func TestListenerCloseUnblocksAccept(t *testing.T) {
	ln := newLoopbackListener(t)

	type acceptResult struct {
		conn *SCTPConn
		err  error
	}
	resCh := make(chan acceptResult, 1)
	go func() {
		c, err := ln.AcceptSCTP()
		resCh <- acceptResult{c, err}
	}()

	// Give the goroutine a moment to block inside accept4.
	time.Sleep(50 * time.Millisecond)

	if err := ln.Close(); err != nil {
		t.Fatalf("first Close returned %v, want nil", err)
	}

	select {
	case res := <-resCh:
		if res.err != ErrListenerClosed {
			t.Errorf("AcceptSCTP after Close returned err=%v, want ErrListenerClosed", res.err)
		}
		if res.conn != nil {
			t.Errorf("AcceptSCTP after Close returned non-nil conn=%+v, want nil", res.conn)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcceptSCTP did not return within 2s after Close")
	}
}

// TestAcceptAfterCloseReturnsErrClosed verifies that calling AcceptSCTP on an
// already-closed listener returns ErrListenerClosed with a nil conn, instead
// of invoking accept4 on a stale (possibly reused) fd.
func TestAcceptAfterCloseReturnsErrClosed(t *testing.T) {
	ln := newLoopbackListener(t)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	conn, err := ln.AcceptSCTP()
	if err != ErrListenerClosed {
		t.Errorf("AcceptSCTP on closed listener err=%v, want ErrListenerClosed", err)
	}
	if conn != nil {
		t.Errorf("AcceptSCTP on closed listener conn=%+v, want nil", conn)
	}
}

// TestDoubleCloseSafe verifies that calling Close twice does not attempt to
// close the underlying fd twice (which could close a different, reused fd).
// The first call returns nil; the second returns syscall.EBADF.
func TestDoubleCloseSafe(t *testing.T) {
	ln := newLoopbackListener(t)

	if err := ln.Close(); err != nil {
		t.Fatalf("first Close returned %v, want nil", err)
	}

	if err := ln.Close(); err != syscall.EBADF {
		t.Errorf("second Close returned %v, want syscall.EBADF", err)
	}
}

// TestConcurrentCloseSingleCloser verifies that many concurrent Close calls
// result in exactly one successful close (returns nil) and the rest return
// syscall.EBADF — guarding against the OS reassigning the fd between calls.
func TestConcurrentCloseSingleCloser(t *testing.T) {
	ln := newLoopbackListener(t)

	const n = 16
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		others    []error
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			err := ln.Close()
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else {
				others = append(others, err)
			}
		}()
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("got %d successful Close calls, want exactly 1 (others=%v)", successes, others)
	}
	for _, err := range others {
		if err != syscall.EBADF {
			t.Errorf("concurrent Close returned %v, want syscall.EBADF", err)
		}
	}
}

// TestAddrAfterCloseReturnsNil verifies that Addr() on a closed listener does
// not call sctpGetAddrs on a stale fd number — it should return nil because
// our fd accessor reports -1.
func TestAddrAfterCloseReturnsNil(t *testing.T) {
	ln := newLoopbackListener(t)

	if a := ln.Addr(); a == nil {
		t.Fatalf("Addr() before Close returned nil, want a real address")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}

	if a := ln.Addr(); a != nil {
		t.Errorf("Addr() after Close returned %v, want nil", a)
	}
}
