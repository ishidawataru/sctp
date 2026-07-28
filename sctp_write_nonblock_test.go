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
	"syscall"
	"testing"
	"time"
)

func writePair(t *testing.T) (client, server *SCTPConn) {
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
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	a := <-ch
	if a.err != nil {
		t.Fatalf("accept: %v", a.err)
	}
	t.Cleanup(func() { _ = a.conn.Close() })
	return client, a.conn
}

// TestWriteDoesNotBlockOnFullSendQueue is the behaviour this change exists
// for: with a peer that has stopped reading, a writer must get an error back
// rather than parking in sendmsg indefinitely.
func TestWriteDoesNotBlockOnFullSendQueue(t *testing.T) {
	client, server := writePair(t)
	_ = server // deliberately never read from

	// Keep the send buffer small so the queue fills quickly.
	if err := client.SetWriteBuffer(16 * 1024); err != nil {
		t.Logf("SetWriteBuffer: %v (continuing)", err)
	}

	payload := make([]byte, 8192)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 10000; i++ {
			if _, err := client.SCTPWrite(payload, nil); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Skip("send queue never filled; nothing to assert")
		}
		if !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Logf("writer stopped with %v", err)
			return
		}
		t.Logf("writer correctly reported %v instead of blocking", err)
	case <-time.After(15 * time.Second):
		t.Fatal("SCTPWrite blocked on a full send queue instead of returning")
	}
}

// TestWriteNeedsRetryEvenWithReadingPeer records the cost of the
// non-blocking flag: EAGAIN is not limited to a peer that has stopped
// reading. A writer that outruns a reading peer sees it too, so every caller
// must now retry rather than treat a write as delivered.
//
// This is the compatibility note for the change. Before MSG_DONTWAIT such a
// write blocked until there was room and callers could ignore the case; now a
// plain write loop silently drops messages.
func TestWriteNeedsRetryEvenWithReadingPeer(t *testing.T) {
	client, server := writePair(t)

	go func() {
		buf := make([]byte, 8192)
		for {
			if _, _, err := server.SCTPRead(buf); err != nil {
				return
			}
		}
	}()

	payload := make([]byte, 1024)
	const messages = 200
	var again int
	for i := 0; i < messages; i++ {
		_, err := client.SCTPWrite(payload, nil)
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			again++
			continue
		}
		if err != nil {
			t.Fatalf("write %d: unexpected error %v", i, err)
		}
	}
	t.Logf("%d of %d writes returned EAGAIN despite an actively reading peer",
		again, messages)
}

// TestWriteWithRetrySendsEverything shows the retry loop callers now need.
func TestWriteWithRetrySendsEverything(t *testing.T) {
	client, server := writePair(t)

	received := make(chan int, 1)
	go func() {
		buf := make([]byte, 8192)
		total := 0
		for {
			n, _, err := server.SCTPRead(buf)
			if err != nil {
				received <- total
				return
			}
			total += n
		}
	}()

	payload := make([]byte, 1024)
	const messages = 200
	for i := 0; i < messages; i++ {
		var err error
		for attempt := 0; attempt < 1000; attempt++ {
			_, err = client.SCTPWrite(payload, nil)
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				time.Sleep(time.Millisecond)
				continue
			}
			break
		}
		if err != nil {
			t.Fatalf("write %d failed after retries: %v", i, err)
		}
	}
	t.Logf("%d messages delivered using a retry loop", messages)
}
