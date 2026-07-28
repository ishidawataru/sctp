//go:build linux

// Drives the close paths so a packet capture can show what each one puts on
// the wire: Close must complete a SHUTDOWN handshake, Abort must send ABORT,
// and Close against an unresponsive peer must fall back to ABORT rather than
// hanging.
//
// Each mode runs on its own port so the capture can be split by association.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/ishidawataru/sctp"
)

func dialPair(port int) (*sctp.SCTPConn, *sctp.SCTPListener, chan *sctp.SCTPConn) {
	addr, err := sctp.ResolveSCTPAddr("sctp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		panic(err)
	}
	ln, err := sctp.ListenSCTP("sctp", addr)
	if err != nil {
		panic(err)
	}
	accepted := make(chan *sctp.SCTPConn, 1)
	go func() {
		c, err := ln.AcceptSCTP()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	conn, err := sctp.DialSCTP("sctp", nil, ln.Addr().(*sctp.SCTPAddr))
	if err != nil {
		panic(err)
	}
	return conn, ln, accepted
}

func main() {
	mode := flag.String("mode", "close", "close | abort | close-unresponsive")
	port := flag.Int("port", 0, "local port to use")
	flag.Parse()

	conn, ln, accepted := dialPair(*port)
	srv := <-accepted

	// Exchange a message so the association is fully established and the
	// capture shows a real data flow before the teardown.
	if _, err := conn.SCTPWrite([]byte("hello"), nil); err != nil {
		panic(err)
	}
	if srv != nil {
		buf := make([]byte, 64)
		if _, _, err := srv.SCTPRead(buf); err != nil {
			panic(err)
		}
	}
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	var err error
	switch *mode {
	case "close":
		err = conn.Close()
	case "abort":
		err = conn.Abort()
	case "close-unresponsive":
		// Kill the peer's socket without a graceful teardown, so nothing
		// answers the SHUTDOWN and Close must fall back to ABORT.
		if srv != nil {
			_ = srv.Abort()
			srv = nil
		}
		time.Sleep(100 * time.Millisecond)
		err = conn.Close()
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
	elapsed := time.Since(start)

	fmt.Printf("mode=%-20s elapsed=%-14v err=%v\n", *mode, elapsed, err)

	if srv != nil {
		_ = srv.Close()
	}
	_ = ln.Close()
	time.Sleep(200 * time.Millisecond)
}
