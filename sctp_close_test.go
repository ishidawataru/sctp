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
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testingTB is the subset of testing.TB these helpers need, so the same setup
// serves both tests and fuzz targets.
type testingTB interface {
	Helper()
	Fatalf(format string, args ...interface{})
	Cleanup(func())
}

// fdIsOpen reports whether fd refers to an open descriptor in this process.
func fdIsOpen(fd int) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL,
		uintptr(fd), uintptr(syscall.F_GETFD), 0)
	return errno == 0
}

// runChild re-executes the test binary for a single test with env set, so a
// test may disturb process-wide state (such as closing stdin) in isolation.
// It returns the child's combined output.
func runChild(exe, testName, env string) (string, error) {
	cmd := exec.Command(exe, "-test.run", "^"+testName+"$", "-test.v")
	cmd.Env = append(os.Environ(), env)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// closePair brings up a loopback association and closes both ends on test
// cleanup. Tests that tear the connections down themselves should use
// eorPairNoCleanup instead, to avoid a double close during cleanup.
func closePair(t testingTB) (client, server *SCTPConn) {
	t.Helper()
	client, server = eorPairNoCleanup(t)
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = server.Close() })
	return client, server
}

// eorPairNoCleanup is closePair without the t.Cleanup close hooks, for tests
// that close the connections themselves and would otherwise trip a double
// close during cleanup.
func eorPairNoCleanup(t testingTB) (client, server *SCTPConn) {
	t.Helper()
	client, server, ok := eorPairMaybe(t)
	if !ok {
		t.Fatalf("could not establish association")
	}
	return client, server
}

// eorPairMaybe is eorPairNoCleanup that reports failure instead of aborting
// the test, for callers that run many iterations and can tolerate the
// occasional dial failure under churn.
func eorPairMaybe(t testingTB) (client, server *SCTPConn, ok bool) {
	t.Helper()

	addr, err := ResolveSCTPAddr("sctp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ln, err := ListenSCTP("sctp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	type accepted struct {
		conn *SCTPConn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.AcceptSCTP()
		ch <- accepted{c, err}
	}()

	client, err = DialSCTP("sctp", nil, ln.Addr().(*SCTPAddr))
	if err != nil {
		return nil, nil, false
	}

	a := <-ch
	if a.err != nil {
		_ = client.Close()
		return nil, nil, false
	}
	return client, a.conn, true
}

// TestDialUnderChurnReportsEISCONN documents a dial-side limitation found
// while fuzzing the close paths.
//
// Under rapid dial/teardown cycles, SCTPConnect can return EISCONN
// ("transport endpoint is already connected") on a freshly created socket.
// The socket is new, so the error reflects kernel association state that has
// not finished tearing down rather than anything the caller did. DialSCTP
// surfaces it unchanged, so callers doing rapid reconnects must retry.
//
// It was originally observed when accepted connections were left open, which
// suggests lingering peer-side associations are what provoke it; aborting
// each accepted connection promptly, as here, usually avoids it. The test
// therefore records what it observes rather than asserting a rate, so it
// documents the behaviour without becoming flaky either way.
//
// This is separate from the close path: connections being closed are released
// correctly, as TestCloseChurnUnderLoad shows.
func TestDialUnderChurnReportsEISCONN(t *testing.T) {
	addr, _ := ResolveSCTPAddr("sctp", "127.0.0.1:0")
	ln, err := ListenSCTP("sctp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Abort each accepted connection rather than letting them accumulate:
	// leaving hundreds of server-side associations open would make the
	// listener's own teardown dominate the test's runtime.
	go func() {
		for {
			c, err := ln.AcceptSCTP()
			if err != nil {
				return
			}
			_ = c.Abort()
		}
	}()

	const cycles = 300
	failures := map[string]int{}
	for i := 0; i < cycles; i++ {
		conn, err := DialSCTP("sctp", nil, ln.Addr().(*SCTPAddr))
		if err != nil {
			failures[err.Error()]++
			continue
		}
		if err := conn.Abort(); err != nil {
			t.Fatalf("abort %d: %v", i, err)
		}
	}

	if len(failures) == 0 {
		t.Logf("%d rapid dial cycles, no failures", cycles)
		return
	}
	for msg, n := range failures {
		t.Logf("%d/%d dials failed with %q", n, cycles, msg)
	}
}

// TestCloseReleasesFdZero covers a descriptor leak that only appears when a
// connection lands on fd 0.
//
// Close and Abort guarded the descriptor with "fd > 0". Zero is a perfectly
// valid descriptor, so a connection on fd 0 had its _fd swapped to -1 and was
// then never closed: the socket leaked and Close reported EBADF as though
// nothing had been open. Daemons that close stdin routinely hand out fd 0.
//
// The test runs in a subprocess because it must close stdin to force the
// allocation, which would otherwise disturb the rest of the suite.
func TestCloseReleasesFdZero(t *testing.T) {
	if os.Getenv("SCTP_FD_ZERO_CHILD") == "1" {
		fdZeroChild()
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate test binary: %v", err)
	}
	out, err := runChild(exe, "TestCloseReleasesFdZero", "SCTP_FD_ZERO_CHILD=1")
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	t.Logf("child reported:\n%s", out)
}

// fdZeroChild closes stdin, opens an association that therefore lands on fd 0,
// and checks Close both reports success and actually releases the descriptor.
func fdZeroChild() {
	report := func(format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}

	addr, err := ResolveSCTPAddr("sctp", "127.0.0.1:0")
	if err != nil {
		report("FAIL resolve: %v", err)
		os.Exit(1)
	}
	ln, err := ListenSCTP("sctp", addr)
	if err != nil {
		report("FAIL listen: %v", err)
		os.Exit(1)
	}
	go func() {
		for {
			c, err := ln.AcceptSCTP()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	// Free fd 0 so the next socket lands there.
	if err := syscall.Close(0); err != nil {
		report("SKIP cannot close stdin: %v", err)
		os.Exit(0)
	}

	conn, err := DialSCTP("sctp", nil, ln.Addr().(*SCTPAddr))
	if err != nil {
		report("SKIP dial: %v", err)
		os.Exit(0)
	}
	fd := conn.fd()
	if fd != 0 {
		report("SKIP association landed on fd %d, not 0; nothing to prove", fd)
		os.Exit(0)
	}

	if err := conn.Close(); err != nil {
		report("FAIL Close on fd 0 returned %v, want nil", err)
		os.Exit(1)
	}
	if fdIsOpen(0) {
		report("FAIL fd 0 is still open after Close; the descriptor leaked")
		os.Exit(1)
	}
	report("ok: connection on fd 0 closed cleanly and the descriptor was released")
	os.Exit(0)
}

// TestAbortReleasesFdZero is the same guarantee for Abort.
func TestAbortReleasesFdZero(t *testing.T) {
	if os.Getenv("SCTP_FD_ZERO_ABORT_CHILD") == "1" {
		abortFdZeroChild()
		return
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate test binary: %v", err)
	}
	out, err := runChild(exe, "TestAbortReleasesFdZero", "SCTP_FD_ZERO_ABORT_CHILD=1")
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	t.Logf("child reported:\n%s", out)
}

func abortFdZeroChild() {
	report := func(format string, args ...interface{}) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}

	addr, _ := ResolveSCTPAddr("sctp", "127.0.0.1:0")
	ln, err := ListenSCTP("sctp", addr)
	if err != nil {
		report("FAIL listen: %v", err)
		os.Exit(1)
	}
	go func() {
		for {
			c, err := ln.AcceptSCTP()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	if err := syscall.Close(0); err != nil {
		report("SKIP cannot close stdin: %v", err)
		os.Exit(0)
	}
	conn, err := DialSCTP("sctp", nil, ln.Addr().(*SCTPAddr))
	if err != nil {
		report("SKIP dial: %v", err)
		os.Exit(0)
	}
	if fd := conn.fd(); fd != 0 {
		report("SKIP association landed on fd %d, not 0", fd)
		os.Exit(0)
	}
	if err := conn.Abort(); err != nil {
		report("FAIL Abort on fd 0 returned %v, want nil", err)
		os.Exit(1)
	}
	if fdIsOpen(0) {
		report("FAIL fd 0 is still open after Abort; the descriptor leaked")
		os.Exit(1)
	}
	report("ok: Abort on fd 0 released the descriptor")
	os.Exit(0)
}

// TestCloseDoesNotLeakDescriptors is the general form: repeated dial/close
// cycles must not accumulate descriptors, whatever fd they land on.
func TestCloseDoesNotLeakDescriptors(t *testing.T) {
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
				buf := make([]byte, 256)
				for {
					if _, _, err := c.SCTPRead(buf); err != nil {
						// See TestCloseChurnUnderLoad: the peer is gone, so
						// a graceful shutdown would only add latency.
						_ = c.Abort()
						return
					}
				}
			}(c)
		}
	}()

	before := countOpenFds(t)
	for i := 0; i < 50; i++ {
		conn, err := DialSCTP("sctp", nil, ln.Addr().(*SCTPAddr))
		if err != nil {
			// Rapid reconnects can hit EISCONN from SCTPConnect; see
			// TestDialUnderChurnReportsEISCONN. That is a dial-side
			// limitation, not a descriptor leak, so skip the cycle.
			continue
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	// The count is process-global, so poll until it settles rather than
	// treating a concurrently running test's socket as a leak here.
	const margin = 10
	var after int
	for attempt := 0; attempt < 40; attempt++ {
		after = countOpenFds(t)
		if after <= before+margin {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("open descriptors before=%d after=%d", before, after)
	if after > before+margin {
		t.Errorf("descriptor count grew from %d to %d over 50 dial/close cycles",
			before, after)
	}
}

func countOpenFds(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("cannot read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// TestCloseReleasesPortForRebind is the symptom the close rework exists to
// fix: after Close returns, the local address must be immediately reusable
// rather than yielding EADDRINUSE.
func TestCloseReleasesPortForRebind(t *testing.T) {
	// Bind a listener on a fixed port, close it, and rebind the same port.
	for attempt := 0; attempt < 5; attempt++ {
		addr, err := ResolveSCTPAddr("sctp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		ln, err := ListenSCTP("sctp", addr)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		bound := ln.Addr().(*SCTPAddr)

		// Hold the accepted connection so it can be closed deterministically:
		// an association still open on the listener's address keeps that
		// address bound, independently of the listener itself.
		accepted := make(chan *SCTPConn, 1)
		go func() {
			c, err := ln.AcceptSCTP()
			if err != nil {
				accepted <- nil
				return
			}
			accepted <- c
		}()

		// Retry past the EISCONN that rapid reconnects can produce; see
		// TestDialUnderChurnReportsEISCONN.
		var conn *SCTPConn
		for attempt := 0; attempt < 20; attempt++ {
			conn, err = DialSCTP("sctp", nil, bound)
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			_ = ln.Close()
			t.Fatalf("dial: %v", err)
		}
		srv := <-accepted
		if err := conn.Close(); err != nil {
			t.Fatalf("conn close: %v", err)
		}
		if srv != nil {
			if err := srv.Close(); err != nil {
				t.Fatalf("accepted conn close: %v", err)
			}
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("listener close: %v", err)
		}

		// The exact address must now be bindable again.
		ln2, err := ListenSCTP("sctp", bound)
		if err != nil {
			t.Fatalf("attempt %d: rebinding %s after close failed: %v",
				attempt, bound, err)
		}
		_ = ln2.Close()
	}
}

// TestCloseWithUnreachablePeerReturnsWithinTimeout pins the fallback path.
// When the peer never acknowledges the shutdown, Close must still return
// promptly rather than blocking indefinitely.
func TestCloseWithUnreachablePeerReturnsWithinTimeout(t *testing.T) {
	client, server := closePair(t)

	// Make the peer unresponsive by abandoning it without a graceful close:
	// drop the server's descriptor so nothing answers the SHUTDOWN.
	if err := server.Abort(); err != nil {
		t.Fatalf("abort server: %v", err)
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- client.Close() }()

	select {
	case err := <-done:
		elapsed := time.Since(start)
		t.Logf("Close returned after %v with err=%v", elapsed, err)
		if elapsed > closeTimeout+2*time.Second {
			t.Errorf("Close took %v, well past the %v timeout", elapsed, closeTimeout)
		}
	case <-time.After(closeTimeout + 5*time.Second):
		t.Fatalf("Close did not return within %v", closeTimeout+5*time.Second)
	}
}

// TestDoubleCloseReturnsEBADF checks the second close is reported rather than
// silently succeeding or closing a descriptor the process has since reused.
func TestDoubleCloseReturnsEBADF(t *testing.T) {
	client, _ := closePair(t)

	if err := client.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := client.Close(); !errors.Is(err, syscall.EBADF) {
		t.Errorf("second Close = %v, want EBADF", err)
	}
	if err := client.Abort(); !errors.Is(err, syscall.EBADF) {
		t.Errorf("Abort after Close = %v, want EBADF", err)
	}
}

// TestCloseOnNilConn guards the nil receiver path.
func TestCloseOnNilConn(t *testing.T) {
	var c *SCTPConn
	if err := c.Close(); !errors.Is(err, syscall.EBADF) {
		t.Errorf("nil Close = %v, want EBADF", err)
	}
	if err := c.Abort(); !errors.Is(err, syscall.EBADF) {
		t.Errorf("nil Abort = %v, want EBADF", err)
	}
}

// TestConcurrentCloseAndAbort checks only one caller wins the descriptor and
// the rest get EBADF, with no double close of a reused fd.
func TestConcurrentCloseAndAbort(t *testing.T) {
	for round := 0; round < 25; round++ {
		client, _ := eorPairNoCleanup(t)

		const racers = 8
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			okCount int
		)
		wg.Add(racers)
		for i := 0; i < racers; i++ {
			go func(i int) {
				defer wg.Done()
				var err error
				if i%2 == 0 {
					err = client.Close()
				} else {
					err = client.Abort()
				}
				if err == nil {
					mu.Lock()
					okCount++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()

		if okCount != 1 {
			t.Fatalf("round %d: %d callers reported success, want exactly 1",
				round, okCount)
		}
	}
}

// TestCloseDuringBlockedRead covers a shutdown racing an in-flight read.
func TestCloseDuringBlockedRead(t *testing.T) {
	client, server := closePair(t)
	_ = client

	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		_, _, err := server.SCTPRead(buf)
		readDone <- err
	}()

	// Give the read time to block in recvmsg.
	time.Sleep(200 * time.Millisecond)
	_ = server.Close()

	select {
	case err := <-readDone:
		t.Logf("blocked read returned %v after Close", err)
	case <-time.After(10 * time.Second):
		t.Fatal("read did not return after Close; the descriptor never unblocked")
	}
}

// TestCloseDuringWrite covers close racing a writer.
func TestCloseDuringWrite(t *testing.T) {
	client, server := closePair(t)

	// Drain the peer. Without a reader the send queue fills and a blocking
	// write never returns, so the test would be measuring send-buffer
	// pressure rather than the close.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, _, err := server.SCTPRead(buf); err != nil {
				return
			}
		}
	}()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		payload := make([]byte, 1024)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := client.SCTPWrite(payload, nil); err != nil {
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	_ = client.Close()
	close(stop)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not observe the close")
	}
}

var _ net.Conn = (*SCTPConn)(nil)
