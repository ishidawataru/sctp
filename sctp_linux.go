// +build linux,!386

package sctp

import (
	"io"
	"net"
	"sync/atomic"
	"syscall"
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

func (c *SCTPConn) SCTPWrite(b []byte, info *SndRcvInfo) (int, error) {
	var cbuf []byte
	if info != nil {
		cmsgBuf := toBuf(info)
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
				return (*SndRcvInfo)(unsafe.Pointer(&m.Data[0])), nil
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

func (c *SCTPConn) Close() error {
	if c != nil {
		fd := atomic.SwapInt32(&c._fd, -1)
		if fd > 0 {
			info := &SndRcvInfo{
				Flags: SCTP_EOF,
			}
			c.SCTPWrite(nil, info)
			syscall.Shutdown(int(fd), syscall.SHUT_RDWR)
			return syscall.Close(int(fd))
		}
	}
	return syscall.EBADF
}

func ListenSCTP(net string, laddr *SCTPAddr) (*SCTPListener, error) {
	return ListenSCTPExt(net, laddr, InitMsg{NumOstreams: SCTP_MAX_STREAM})
}

func ListenSCTPExt(network string, laddr *SCTPAddr, options InitMsg) (*SCTPListener, error) {
	af, ipv6only := favoriteAddrFamily(network, laddr, nil, "listen")
	sock, err := syscall.Socket(
		af,
		syscall.SOCK_STREAM,
		syscall.IPPROTO_SCTP,
	)
	if err != nil {
		return nil, err
	}
	if err = setDefaultSockopts(sock, af, ipv6only); err != nil {
		syscall.Close(sock)
		return nil, err
	}
	err = setInitOpts(sock, options)
	if err != nil {
		syscall.Close(sock)
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
		err := SCTPBind(sock, laddr, SCTP_BINDX_ADD_ADDR)
		if err != nil {
			syscall.Close(sock)
			return nil, err
		}
	}
	err = syscall.Listen(sock, syscall.SOMAXCONN)
	if err != nil {
		syscall.Close(sock)
		return nil, err
	}
	return &SCTPListener{
		fd: sock,
	}, nil
}

func (ln *SCTPListener) Accept() (net.Conn, error) {
	fd, _, err := syscall.Accept4(ln.fd, 0)
	return NewSCTPConn(fd, nil), err
}

func (ln *SCTPListener) Close() error {
	syscall.Shutdown(ln.fd, syscall.SHUT_RDWR)
	return syscall.Close(ln.fd)
}

func DialSCTP(net string, laddr, raddr *SCTPAddr) (*SCTPConn, error) {
	return DialSCTPExt(net, laddr, raddr, InitMsg{NumOstreams: SCTP_MAX_STREAM})
}

func DialSCTPExt(network string, laddr, raddr *SCTPAddr, options InitMsg) (*SCTPConn, error) {
	af, ipv6only := favoriteAddrFamily(network, laddr, raddr, "dial")
	sock, err := syscall.Socket(
		af,
		syscall.SOCK_STREAM,
		syscall.IPPROTO_SCTP,
	)
	if err != nil {
		return nil, err
	}
	if err = setDefaultSockopts(sock, af, ipv6only); err != nil {
		syscall.Close(sock)
		return nil, err
	}
	err = setInitOpts(sock, options)
	if err != nil {
		syscall.Close(sock)
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
		err := SCTPBind(sock, laddr, SCTP_BINDX_ADD_ADDR)
		if err != nil {
			syscall.Close(sock)
			return nil, err
		}
	}
	_, err = SCTPConnect(sock, raddr)
	if err != nil {
		syscall.Close(sock)
		return nil, err
	}
	return NewSCTPConn(sock, nil), nil
}
