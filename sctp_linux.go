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
	"io"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

func setsockopt(fd int, optname, optval, optlen uintptr) (uintptr, uintptr, error) {
	// FIXME: syscall.SYS_SETSOCKOPT is undefined on 386
	r0, r1, errno := syscall.Syscall6(syscall.SYS_SETSOCKOPT,
		uintptr(fd),
		SOL_SCTP,
		optname,
		optval,
		optlen,
		0)
	if errno != 0 {
		return r0, r1, errno
	}
	return r0, r1, nil
}

func getsockopt(fd int, optname, optval, optlen uintptr) (uintptr, uintptr, error) {
	if runtime.GOARCH == "s390x" {
		optlen = uintptr(unsafe.Pointer(&optlen))
	}
	// FIXME: syscall.SYS_GETSOCKOPT is undefined on 386
	r0, r1, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT,
		uintptr(fd),
		SOL_SCTP,
		optname,
		optval,
		optlen,
		0)
	if errno != 0 {
		return r0, r1, errno
	}
	return r0, r1, nil
}

type rawConn struct {
	sockfd int
}

func (r rawConn) Control(f func(fd uintptr)) error {
	f(uintptr(r.sockfd))
	return nil
}

func (r rawConn) Read(f func(fd uintptr) (done bool)) error {
	panic("not implemented")
}

func (r rawConn) Write(f func(fd uintptr) (done bool)) error {
	panic("not implemented")
}

func (c *SCTPConn) SyscallConn() (syscall.RawConn, error) {
	fd := c.fd()
	if fd < 0 {
		return nil, syscall.EINVAL
	}
	return &rawConn{sockfd: int(fd)}, nil
}

func (c *SCTPConn) SCTPWrite(b []byte, info *SndRcvInfo) (int, error) {
	var cbuf []byte
	if info != nil {
		// Fix PPID to network byte order
		oldPPID := info.PPID
		info.PPID = htonl(info.PPID)
		cmsgBuf := toBuf(info)
		info.PPID = oldPPID
		hdr := &syscall.Cmsghdr{
			Level: syscall.IPPROTO_SCTP,
			Type:  SCTP_CMSG_SNDRCV,
		}

		// bitwidth of hdr.Len is platform-specific,
		// so we use hdr.SetLen() rather than directly setting hdr.Len
		hdr.SetLen(syscall.CmsgSpace(len(cmsgBuf)))
		cbuf = append(toBuf(hdr), cmsgBuf...)
	}
	return syscall.SendmsgN(c.fd(), b, cbuf, nil, 0)
}

func parseSndRcvInfo(b []byte) (*SndRcvInfo, error) {
	msgs, err := syscall.ParseSocketControlMessage(b)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.Header.Level == syscall.IPPROTO_SCTP {
			switch m.Header.Type {
			case SCTP_CMSG_SNDRCV:
				dst := (*SndRcvInfo)(unsafe.Pointer(&m.Data[0]))
				// Fix PPID to host byte order
				dst.PPID = ntohl(dst.PPID)
				return dst, nil
			}
		}
	}
	return nil, nil
}

func (c *SCTPConn) SCTPRead(b []byte) (int, *SndRcvInfo, error) {
	oob := make([]byte, 254)
	for {
		n, oobn, recvflags, _, err := syscall.Recvmsg(c.fd(), b, oob, 0)
		if err != nil {
			return n, nil, err
		}

		if n == 0 && oobn == 0 {
			return 0, nil, io.EOF
		}

		if recvflags&MSG_NOTIFICATION > 0 && c.notificationHandler != nil {
			if err := c.notificationHandler(b[:n]); err != nil {
				return 0, nil, err
			}
		} else {
			var info *SndRcvInfo
			if oobn > 0 {
				info, err = parseSndRcvInfo(oob[:oobn])
			}
			return n, info, err
		}
	}
}

// Close closes the SCTP connection gracefully with a timeout fallback.
//
// It initiates a graceful shutdown by sending a SHUTDOWN chunk to the peer
// and waits up to 3 seconds for the peer to acknowledge (SHUTDOWN-ACK).
// If the peer responds, the connection closes gracefully and resources are
// released immediately. If the peer does not respond within the timeout
// (e.g., network failure or unreachable peer), an ABORT chunk is sent to
// forcefully terminate the association and release resources.
//
// This ensures that Close always returns promptly and releases resources,
// avoiding the EADDRINUSE "Address already in use" error that can occur when the kernel
// still occupies the resource.
//
// For immediate termination without waiting, use Abort() instead.
func (c *SCTPConn) Close() error {
	return c.CloseWithTimeout(closeTimeout)
}

// closeTimeout is how long Close waits for the peer to acknowledge a
// shutdown before falling back to an ABORT. It is a compromise: long enough
// that a peer on a congested link can still complete the handshake, short
// enough that a server tearing down many associations is not held up by a
// peer that will never answer. Use CloseWithTimeout to choose another value.
const closeTimeout = 3 * time.Second

// establishTimeout bounds the same wait on the error paths of dial and
// listen. Those sockets have no established association to shut down
// gracefully, so the wait exists only to let the kernel release the address.
const establishTimeout = 1 * time.Second

// CloseWithTimeout is Close with a caller-chosen grace period.
//
// A zero or negative timeout skips the wait entirely and terminates the
// association immediately, which is equivalent to Abort.
func (c *SCTPConn) CloseWithTimeout(timeout time.Duration) error {
	if c == nil {
		return syscall.EBADF
	}
	fd := atomic.SwapInt32(&c._fd, -1)
	// Zero is a valid descriptor: a process that has closed stdin can be
	// handed fd 0 for a socket. Guarding with "fd > 0" leaked it.
	if fd < 0 {
		return syscall.EBADF
	}
	return closeSctpSocket(int(fd), timeout)
}

func closeSctpSocket(fd int, timeout time.Duration) error {
	if timeout <= 0 {
		return abortSctpSocket(fd)
	}

	// Send SHUTDOWN to initiate graceful shutdown. A failure here means no
	// graceful shutdown is possible, so skip the wait and abort instead of
	// blocking for a SHUTDOWN-ACK that cannot arrive.
	if err := syscall.Shutdown(fd, syscall.SHUT_RDWR); err != nil {
		return abortSctpSocket(fd)
	}

	// Wait for graceful shutdown to complete.
	// If peer responds, Read returns immediately with ENOTCONN.
	// If peer is unreachable, Read times out after the configured duration.
	//
	// The timeout is what bounds this read, so if it cannot be programmed the
	// read would block indefinitely. Abort rather than risk that.
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO,
		&tv); err != nil {
		return abortSctpSocket(fd)
	}
	var buf [1]byte
	// The result is deliberately ignored: this read exists to wait for the
	// peer's SHUTDOWN-ACK, and any outcome (data, ENOTCONN, timeout) means
	// the wait is over.
	_, _ = syscall.Read(fd, buf[:])

	// Set linger=0 so close() sends ABORT if handshake didn't complete,
	// or just releases resources if it did.
	if err := syscall.SetsockoptLinger(fd, syscall.SOL_SOCKET, syscall.SO_LINGER,
		&syscall.Linger{Onoff: 1, Linger: 0}); err != nil {
		// Without linger the close may leave the association lingering, but
		// the descriptor must still be released.
		if cerr := syscall.Close(fd); cerr != nil {
			return cerr
		}
		return err
	}
	return syscall.Close(fd)
}

// abortSctpSocket terminates the association immediately and releases fd.
func abortSctpSocket(fd int) error {
	// Setting SO_LINGER with l_onoff=1 and l_linger=0 causes the kernel to
	// send an ABORT chunk instead of SHUTDOWN when closing.
	lerr := syscall.SetsockoptLinger(fd, syscall.SOL_SOCKET, syscall.SO_LINGER,
		&syscall.Linger{Onoff: 1, Linger: 0})
	if err := syscall.Close(fd); err != nil {
		return err
	}
	return lerr
}

// Abort terminates the SCTP association immediately by sending an ABORT chunk.
// Unlike Close(), this does not perform a graceful shutdown handshake.
// Use this when you need immediate resource release without waiting for
// the peer to acknowledge the shutdown (e.g., when the peer is unreachable).
func (c *SCTPConn) Abort() error {
	if c == nil {
		return syscall.EBADF
	}
	fd := atomic.SwapInt32(&c._fd, -1)
	// See CloseWithTimeout: fd 0 is valid and must not be skipped.
	if fd < 0 {
		return syscall.EBADF
	}
	return abortSctpSocket(int(fd))
}

func (c *SCTPConn) SetWriteBuffer(bytes int) error {
	return syscall.SetsockoptInt(c.fd(), syscall.SOL_SOCKET, syscall.SO_SNDBUF, bytes)
}

func (c *SCTPConn) GetWriteBuffer() (int, error) {
	return syscall.GetsockoptInt(c.fd(), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
}

func (c *SCTPConn) SetReadBuffer(bytes int) error {
	return syscall.SetsockoptInt(c.fd(), syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}

func (c *SCTPConn) GetReadBuffer() (int, error) {
	return syscall.GetsockoptInt(c.fd(), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
}

// ListenSCTP - start listener on specified address/port
func ListenSCTP(net string, laddr *SCTPAddr) (*SCTPListener, error) {
	return ListenSCTPExt(net, laddr, InitMsg{NumOstreams: SCTP_MAX_STREAM})
}

// ListenSCTPExt - start listener on specified address/port with given SCTP options
func ListenSCTPExt(network string, laddr *SCTPAddr, options InitMsg) (*SCTPListener, error) {
	return listenSCTPExtConfig(network, laddr, options, nil, nil)
}

// listenSCTPExtConfig - start listener on specified address/port with given SCTP options and socket configuration
func listenSCTPExtConfig(network string, laddr *SCTPAddr, options InitMsg, control func(network, address string, c syscall.RawConn) error, notificationHandler NotificationHandler) (*SCTPListener, error) {
	af, ipv6only := favoriteAddrFamily(network, laddr, nil, "listen")
	sock, err := syscall.Socket(
		af,
		syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC,
		syscall.IPPROTO_SCTP,
	)
	if err != nil {
		return nil, err
	}

	// close socket on error
	defer func() {
		if err != nil {
			_ = closeSctpSocket(sock, establishTimeout)
		}
	}()
	if err = setDefaultSockopts(sock, af, ipv6only); err != nil {
		return nil, err
	}
	if control != nil {
		rc := rawConn{sockfd: sock}
		var localAddressString string
		if laddr != nil {
			localAddressString = laddr.String()
		}
		if err = control(network, localAddressString, rc); err != nil {
			return nil, err
		}
	}
	err = setInitOpts(sock, options)
	if err != nil {
		return nil, err
	}

	if laddr != nil {
		// If IP address and/or port was not provided so far, let's use the unspecified IPv4 or IPv6 address
		if len(laddr.IPAddrs) == 0 {
			if af == syscall.AF_INET {
				laddr.IPAddrs = append(laddr.IPAddrs, net.IPAddr{IP: net.IPv4zero})
			} else if af == syscall.AF_INET6 {
				laddr.IPAddrs = append(laddr.IPAddrs, net.IPAddr{IP: net.IPv6zero})
			}
		}
		err = SCTPBind(sock, laddr, SCTP_BINDX_ADD_ADDR)
		if err != nil {
			return nil, err
		}
	}
	err = syscall.Listen(sock, syscall.SOMAXCONN)
	if err != nil {
		return nil, err
	}
	return &SCTPListener{
			fd:                  sock,
			notificationHandler: notificationHandler,
		},
		nil
}

// FileListener takes a file, dup's the underlying file descriptor, and returns
// a SCTPListener created from the dup'd fd.
func FileListener(file *os.File) (*SCTPListener, error) {
	r1, _, err := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), syscall.F_DUPFD_CLOEXEC, 0)
	if err != 0 {
		return nil, os.NewSyscallError("fcntl", err)
	}

	// Clear the non-blocking flag on the dup'd fd. This is needed to make sure
	// an SCTPListener created with FileListener will behave like other
	// listeners. Namely, its Accept methods will block until a connection is
	// available.
	if err := syscall.SetNonblock(int(r1), false); err != nil {
		return nil, os.NewSyscallError("fcntl", err)
	}

	return &SCTPListener{
		fd:                  int(r1),
		notificationHandler: nil,
	}, nil
}

// AcceptSCTP waits for and returns the next SCTP connection to the listener.
func (ln *SCTPListener) AcceptSCTP() (*SCTPConn, error) {
	fd, _, err := syscall.Accept4(ln.fd, 0)
	return NewSCTPConn(fd, ln.notificationHandler), err
}

// Accept waits for and returns the next connection connection to the listener.
func (ln *SCTPListener) Accept() (net.Conn, error) {
	return ln.AcceptSCTP()
}

func (ln *SCTPListener) Close() error {
	syscall.Shutdown(ln.fd, syscall.SHUT_RDWR)
	return syscall.Close(ln.fd)
}

func (ln *SCTPListener) SyscallConn() (syscall.RawConn, error) {
	fd := ln.fd
	if fd < 0 {
		return nil, syscall.EINVAL
	}
	return &rawConn{sockfd: int(fd)}, nil
}

// DialSCTP - bind socket to laddr (if given) and connect to raddr
func DialSCTP(net string, laddr, raddr *SCTPAddr) (*SCTPConn, error) {
	return DialSCTPExt(net, laddr, raddr, InitMsg{NumOstreams: SCTP_MAX_STREAM})
}

// DialSCTPExt - same as DialSCTP but with given SCTP options
func DialSCTPExt(network string, laddr, raddr *SCTPAddr, options InitMsg) (*SCTPConn, error) {
	return dialSCTPExtConfig(network, laddr, raddr, options, nil, nil)
}

// dialSCTPExtConfig - same as DialSCTP but with given SCTP options and socket configuration
func dialSCTPExtConfig(network string, laddr, raddr *SCTPAddr, options InitMsg, control func(network, address string, c syscall.RawConn) error, notificationHandler NotificationHandler) (*SCTPConn, error) {
	af, ipv6only := favoriteAddrFamily(network, laddr, raddr, "dial")
	sock, err := syscall.Socket(
		af,
		syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC,
		syscall.IPPROTO_SCTP,
	)
	if err != nil {
		return nil, err
	}

	// close socket on error
	defer func() {
		if err != nil {
			_ = closeSctpSocket(sock, establishTimeout)
		}
	}()
	if err = setDefaultSockopts(sock, af, ipv6only); err != nil {
		return nil, err
	}
	if control != nil {
		rc := rawConn{sockfd: sock}
		var localAddressString string
		if laddr != nil {
			localAddressString = laddr.String()
		}
		if err = control(network, localAddressString, rc); err != nil {
			return nil, err
		}
	}
	err = setInitOpts(sock, options)
	if err != nil {
		return nil, err
	}
	if laddr != nil {
		// If IP address and/or port was not provided so far, let's use the unspecified IPv4 or IPv6 address
		if len(laddr.IPAddrs) == 0 {
			if af == syscall.AF_INET {
				laddr.IPAddrs = append(laddr.IPAddrs, net.IPAddr{IP: net.IPv4zero})
			} else if af == syscall.AF_INET6 {
				laddr.IPAddrs = append(laddr.IPAddrs, net.IPAddr{IP: net.IPv6zero})
			}
		}
		err = SCTPBind(sock, laddr, SCTP_BINDX_ADD_ADDR) // error EADDRINUSE "Address already in use" may occur if resource (source IP and Port) is occupied
		if err != nil {
			return nil, err
		}
	}
	_, err = SCTPConnect(sock, raddr)
	if err != nil {
		return nil, err
	}
	return NewSCTPConn(sock, notificationHandler), nil
}
