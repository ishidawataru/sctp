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
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sctp

import (
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

// dialLoopback brings up a listener and a connection to it, and cleans both up.
func dialLoopback(t *testing.T, port int) *SCTPConn {
	t.Helper()

	addr := &SCTPAddr{IPAddrs: []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, Port: port}
	listener, err := ListenSCTP("sctp", addr)
	if err != nil {
		t.Skipf("SCTP is not available here: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	conn, err := DialSCTP("sctp", nil, addr)
	if err != nil {
		t.Fatalf("DialSCTP: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// isTimeout reports whether err is a timeout in the sense net.Conn documents:
// "An error returned after a timeout fails will implement the Timeout method,
// and calling the Timeout method will return true."
func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

func TestSetReadDeadlineExpiresABlockedRead(t *testing.T) {
	conn := dialLoopback(t, 54331)

	if err := conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	start := time.Now()
	_, err := conn.Read(make([]byte, 64))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Read returned no error; nothing was ever sent, so it should have expired")
	}
	if !isTimeout(err) {
		t.Fatalf("Read error = %v, want one whose Timeout() is true", err)
	}
	// Generous, because this asserts the read did not return instantly by
	// some other route — not the precision of the kernel timer.
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Read expired after %v, well before the 150ms deadline", elapsed)
	}
}

func TestSetReadDeadlineInThePastExpiresImmediately(t *testing.T) {
	// ⚠️ The trap this guards: a past deadline converts to a non-positive
	// duration, and a zero timeval means "block forever" — so the one case
	// that must not block is the one that would block hardest.
	conn := dialLoopback(t, 54332)

	if err := conn.SetReadDeadline(time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 64))
		done <- err
	}()

	select {
	case err := <-done:
		if !isTimeout(err) {
			t.Fatalf("Read error = %v, want one whose Timeout() is true", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read blocked on a deadline that had already passed")
	}
}

func TestZeroDeadlineRemovesTheTimeout(t *testing.T) {
	conn := dialLoopback(t, 54333)

	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("SetReadDeadline(zero): %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 64))
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("Read returned %v; the deadline was removed, so it should still be blocked", err)
	case <-time.After(500 * time.Millisecond):
		// Still blocked, which is the point.
	}
}

func TestSetDeadlineSetsBoth(t *testing.T) {
	conn := dialLoopback(t, 54334)

	if err := conn.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := conn.Read(make([]byte, 64)); !isTimeout(err) {
		t.Fatalf("Read error = %v, want a timeout; SetDeadline should have set the read half", err)
	}
}

func TestDeadlineSettersNoLongerReturnEOPNOTSUPP(t *testing.T) {
	// The regression this whole change exists to prevent: all three setters
	// used to return syscall.EOPNOTSUPP unconditionally, which made SCTPConn
	// a net.Conn that did not implement net.Conn.
	conn := dialLoopback(t, 54335)

	for name, set := range map[string]func(time.Time) error{
		"SetDeadline":      conn.SetDeadline,
		"SetReadDeadline":  conn.SetReadDeadline,
		"SetWriteDeadline": conn.SetWriteDeadline,
	} {
		if err := set(time.Now().Add(time.Second)); errors.Is(err, syscall.EOPNOTSUPP) {
			t.Errorf("%s returned EOPNOTSUPP", name)
		} else if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDeadlineToTimeval(t *testing.T) {
	if tv := deadlineToTimeval(time.Time{}); tv.Sec != 0 || tv.Usec != 0 {
		t.Errorf("the zero time gave %+v, want a zero timeval meaning no deadline", tv)
	}
	if tv := deadlineToTimeval(time.Now().Add(-time.Hour)); tv.Sec == 0 && tv.Usec == 0 {
		t.Error("a deadline in the past gave a zero timeval, which means block forever")
	}
	if tv := deadlineToTimeval(time.Now().Add(2 * time.Second)); tv.Sec < 1 || tv.Sec > 2 {
		t.Errorf("a 2s deadline gave %+v seconds, want 1 or 2", tv.Sec)
	}
}
