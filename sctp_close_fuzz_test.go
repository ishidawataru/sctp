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
	"sync"
	"syscall"
	"testing"
	"time"
)

// FuzzCloseWithTimeout varies the grace period, including the negative and
// zero values that select the immediate-abort path, and the sub-microsecond
// values where the timeval conversion could round down to "no timeout" and
// block forever.
//
// The invariant: CloseWithTimeout always returns, always releases the
// descriptor, and a second call always reports EBADF.
func FuzzCloseWithTimeout(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(-1))
	f.Add(int64(1))                       // 1ns: rounds up to 1us
	f.Add(int64(999))                     // sub-microsecond
	f.Add(int64(time.Millisecond))        //nolint:gosec
	f.Add(int64(3 * time.Second))         //nolint:gosec
	f.Add(int64(time.Hour))               //nolint:gosec

	f.Fuzz(func(t *testing.T, ns int64) {
		// Keep the wait bounded: this exercises the conversion arithmetic and
		// the branch selection, not real multi-second waits.
		if ns > int64(2*time.Second) {
			ns = int64(2 * time.Second)
		}
		timeout := time.Duration(ns)

		// Rapid dial churn can hit EISCONN from SCTPConnect (see
		// TestDialUnderChurnReportsEISCONN); that is a dial-side limitation,
		// not a close-path failure, so skip rather than fail this iteration.
		client, server, ok := eorPairMaybe(t)
		if !ok {
			t.Skip("association could not be established this iteration")
		}
		defer func() { _ = server.Close() }()

		fd := client.fd()

		done := make(chan error, 1)
		go func() { done <- client.CloseWithTimeout(timeout) }()

		select {
		case err := <-done:
			if err != nil && !errors.Is(err, syscall.EBADF) {
				t.Fatalf("timeout=%v: CloseWithTimeout = %v", timeout, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("timeout=%v: CloseWithTimeout never returned", timeout)
		}

		if fd >= 0 && fdIsOpen(fd) {
			t.Fatalf("timeout=%v: descriptor %d still open after close", timeout, fd)
		}
		if err := client.Close(); !errors.Is(err, syscall.EBADF) {
			t.Fatalf("timeout=%v: second Close = %v, want EBADF", timeout, err)
		}
	})
}

// TestCloseTimeoutZeroIsImmediate pins the documented equivalence between a
// non-positive timeout and Abort.
func TestCloseTimeoutZeroIsImmediate(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		client, server := eorPairNoCleanup(t)

		start := time.Now()
		if err := client.CloseWithTimeout(timeout); err != nil {
			t.Fatalf("timeout=%v: %v", timeout, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("timeout=%v: took %v, expected an immediate abort", timeout, d)
		}
		_ = server.Close()
	}
}

// TestCloseSubMicrosecondTimeout guards the timeval conversion: a timeout
// under one microsecond must not truncate to zero, which the kernel reads as
// "block forever".
func TestCloseSubMicrosecondTimeout(t *testing.T) {
	client, server := eorPairNoCleanup(t)
	defer func() { _ = server.Close() }()

	done := make(chan error, 1)
	go func() { done <- client.CloseWithTimeout(1 * time.Nanosecond) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CloseWithTimeout(1ns) = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("CloseWithTimeout(1ns) blocked; the timeval likely truncated to zero")
	}
}

// TestCloseChurnUnderLoad is the industry case: a signalling node cycling many
// associations must not accumulate descriptors or wedge on teardown.
func TestCloseChurnUnderLoad(t *testing.T) {
	addr, _ := ResolveSCTPAddr("sctp", "127.0.0.1:0")
	ln, err := ListenSCTP("sctp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		for {
			c, err := ln.AcceptSCTP()
			if err != nil {
				return
			}
			go func(c *SCTPConn) {
				buf := make([]byte, 512)
				for {
					if _, _, err := c.SCTPRead(buf); err != nil {
						// The client has already gone; abort rather than
						// run a graceful shutdown that has no peer to
						// answer it and would serialise the whole test.
						_ = c.Abort()
						return
					}
				}
			}(c)
		}
	}()

	before := countOpenFds(t)

	const workers, perWorker = 8, 15
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				conn, err := DialSCTP("sctp", nil, ln.Addr().(*SCTPAddr))
				if err != nil {
					errs <- err
					continue
				}
				// Send something so the association is real before teardown.
				_, _ = conn.SCTPWrite([]byte("churn"), nil)

				// Alternate graceful and immediate teardown. The graceful
				// path uses a short grace period deliberately: the server
				// aborts its side as soon as its read fails, so a SHUTDOWN
				// may find no peer to answer it, and the default period
				// would make this loop wait out the timeout each time.
				if (w+i)%2 == 0 {
					err = conn.CloseWithTimeout(200 * time.Millisecond)
				} else {
					err = conn.Abort()
				}
				if err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	var failures int
	for err := range errs {
		failures++
		if failures <= 3 {
			t.Logf("teardown error: %v", err)
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d cycles failed", failures, workers*perWorker)
	}

	// Wait for the accept goroutines to reap their side. The descriptor count
	// is process-global, so other tests running concurrently in the same
	// binary can inflate it transiently; poll until it settles rather than
	// sampling once and treating a neighbour's socket as a leak here.
	const margin = 20
	var after int
	for attempt := 0; attempt < 40; attempt++ {
		time.Sleep(100 * time.Millisecond)
		after = countOpenFds(t)
		if after <= before+margin {
			break
		}
	}
	t.Logf("descriptors before=%d after=%d over %d cycles",
		before, after, workers*perWorker)
	if after > before+margin {
		t.Errorf("descriptors grew from %d to %d over %d cycles and did not settle",
			before, after, workers*perWorker)
	}
}

// TestCloseRacesWithReadAndWrite drives close against concurrent I/O, the
// shape that surfaces use-after-close on a recycled descriptor.
func TestCloseRacesWithReadAndWrite(t *testing.T) {
	for round := 0; round < 20; round++ {
		client, server := eorPairNoCleanup(t)

		var wg sync.WaitGroup
		wg.Add(3)

		go func() {
			defer wg.Done()
			buf := make([]byte, 256)
			for i := 0; i < 50; i++ {
				if _, _, err := client.SCTPRead(buf); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			payload := []byte("racing")
			for i := 0; i < 50; i++ {
				if _, err := client.SCTPWrite(payload, nil); err != nil {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(round%5) * time.Millisecond)
			_ = client.Close()
		}()

		wg.Wait()
		_ = server.Close()
	}
}

// TestAbortDoesNotWait checks Abort returns promptly even when the peer is
// gone, since it performs no handshake.
func TestAbortDoesNotWait(t *testing.T) {
	client, server := eorPairNoCleanup(t)
	if err := server.Abort(); err != nil {
		t.Fatalf("server abort: %v", err)
	}

	start := time.Now()
	if err := client.Abort(); err != nil {
		t.Fatalf("client abort: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Abort took %v against a dead peer; it must not wait", d)
	}
}
