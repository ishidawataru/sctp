#!/usr/bin/env bash
# Wire-level verification of the close paths.
#
# Confirms on the wire that Close completes a SHUTDOWN handshake, that Abort
# sends an ABORT chunk, and that Close against an unresponsive peer falls back
# to ABORT instead of hanging.
set -euo pipefail

modprobe sctp 2>/dev/null || true
CAP=/tmp/sctp-close.pcap

cd /src/testdata/closeprobe
cat > go.mod <<EOF
module closeprobe
go 1.21
require github.com/ishidawataru/sctp v0.0.0
replace github.com/ishidawataru/sctp => /src
EOF
trap 'rm -f go.mod go.sum' EXIT
go mod tidy >/dev/null 2>&1
go build -o /tmp/closeprobe . >/dev/null

echo "Capturing loopback SCTP..."
tshark -i lo -f "ip proto 132" -w "$CAP" -q >/dev/null 2>&1 &
TPID=$!
sleep 3

# Distinct ports so each teardown can be isolated in the capture.
/tmp/closeprobe -mode close              -port 39001
/tmp/closeprobe -mode abort              -port 39002
/tmp/closeprobe -mode close-unresponsive -port 39003

sleep 2
kill -INT $TPID 2>/dev/null || true
wait $TPID 2>/dev/null || true

# chunk type 7 = SHUTDOWN, 8 = SHUTDOWN_ACK, 14 = SHUTDOWN_COMPLETE, 6 = ABORT
summarise() {
    local port="$1" label="$2"
    echo
    echo "=== ${label} (port ${port}) ==="
    tshark -r "$CAP" -Y "sctp.port == ${port}" -T fields -e sctp.chunk_type \
        2>/dev/null | tr ',' '\n' | grep -v '^$' | sort -n | uniq -c \
        | awk '{
            name = "type " $2;
            if ($2==0) name="DATA"; else if ($2==1) name="INIT";
            else if ($2==2) name="INIT_ACK"; else if ($2==3) name="SACK";
            else if ($2==6) name="ABORT"; else if ($2==7) name="SHUTDOWN";
            else if ($2==8) name="SHUTDOWN_ACK"; else if ($2==10) name="COOKIE_ECHO";
            else if ($2==11) name="COOKIE_ACK"; else if ($2==14) name="SHUTDOWN_COMPLETE";
            printf "  %-18s count=%s\n", name, $1
        }'

    local shutdown abort
    shutdown=$(tshark -r "$CAP" -Y "sctp.port == ${port}" -T fields -e sctp.chunk_type 2>/dev/null \
        | tr ',' '\n' | grep -c '^7$' || true)
    abort=$(tshark -r "$CAP" -Y "sctp.port == ${port}" -T fields -e sctp.chunk_type 2>/dev/null \
        | tr ',' '\n' | grep -c '^6$' || true)
    echo "  -> SHUTDOWN chunks=${shutdown}, ABORT chunks=${abort}"
}

summarise 39001 "Close: expect a SHUTDOWN handshake, no ABORT"
summarise 39002 "Abort: expect an ABORT chunk, no SHUTDOWN"
summarise 39003 "Close with unresponsive peer: expect the ABORT fallback"
