package main

import (
	"anytls/proxy"
	"anytls/proxy/padding"
	"anytls/proxy/session"
	"anytls/user"
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"runtime/debug"
	"strings"

	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/sirupsen/logrus"
)

func handleTcpConnection(ctx context.Context, c net.Conn, s *myServer) {
	defer func() {
		if r := recover(); r != nil {
			logrus.Errorln("[BUG]", r, string(debug.Stack()))
		}
	}()

	c = tls.Server(c, s.tlsConfig)
	defer c.Close()

	b := buf.NewPacket()
	defer b.Release()

	n, err := b.ReadOnceFrom(c)
	if err != nil {
		logrus.Debugln("ReadOnceFrom:", err)
		return
	}
	c = bufio.NewCachedConn(c, b)

	by, err := b.ReadBytes(32)
	if err != nil {
		b.Resize(0, n)
		fallback(ctx, c, s.fallbackAddr)
		return
	}
	var hash [32]byte
	copy(hash[:], by)
	userState, ok := s.userManager.LookupByPasswordHash(hash)
	if !ok {
		b.Resize(0, n)
		fallback(ctx, c, s.fallbackAddr)
		return
	}
	by, err = b.ReadBytes(2)
	if err != nil {
		b.Resize(0, n)
		fallback(ctx, c, s.fallbackAddr)
		return
	}
	paddingLen := binary.BigEndian.Uint16(by)
	if paddingLen > 0 {
		_, err = b.ReadBytes(int(paddingLen))
		if err != nil {
			b.Resize(0, n)
			fallback(ctx, c, s.fallbackAddr)
			return
		}
	}

	if userState.IsExpired() {
		logrus.Debugln("user account expired, rejecting:", userState.Username())
		return
	}
	if userState.IsOverTraffic() {
		logrus.Debugln("user over traffic quota, rejecting:", userState.Username())
		return
	}

	remoteIP, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		remoteIP = c.RemoteAddr().String()
	}
	if !userState.AcquireIP(remoteIP) {
		logrus.Debugln("user IP limit exceeded, rejecting:", userState.Username(), remoteIP)
		return
	}
	defer userState.ReleaseIP(remoteIP)

	sess := session.NewServerSession(c, func(stream *session.Stream) {
		defer func() {
			if r := recover(); r != nil {
				logrus.Errorln("[BUG]", r, string(debug.Stack()))
			}
		}()
		defer stream.Close()

		if userState.IsExpired() {
			logrus.Debugln("user account expired, closing stream:", userState.Username())
			return
		}
		if userState.IsOverTraffic() {
			logrus.Debugln("user over traffic quota, closing stream:", userState.Username())
			return
		}
		if !userState.AcquireConn() {
			logrus.Debugln("user connection limit exceeded:", userState.Username())
			return
		}
		defer userState.ReleaseConn()

		destination, err := M.SocksaddrSerializer.ReadAddrPort(stream)
		if err != nil {
			logrus.Debugln("ReadAddrPort:", err)
			return
		}
		if destination.IsFqdn() {
			if userState.IsDomainBlocked(destination.Fqdn) {
				logrus.Debugln("user domain blocked, rejecting stream:", userState.Username(), destination.Fqdn)
				return
			}
		} else if destination.Addr.IsValid() && userState.IsIPBlocked(destination.Addr) {
			logrus.Debugln("user IP blocked, rejecting stream:", userState.Username(), destination.Addr)
			return
		}

		isUoT := strings.Contains(destination.String(), "udp-over-tcp.arpa")
		if !isUoT {
			// The UoT placeholder destination above isn't a real site - the
			// actual per-packet UDP targets aren't visible at this layer, so
			// only record visits for genuine TCP destinations.
			userState.RecordVisit(destination.AddrString(), remoteIP)
		}

		conn := &countingConn{Stream: stream, state: userState}
		dialer := proxy.DialerForGroup(userState.OutboundGroupID())

		if isUoT {
			proxyOutboundUoT(ctx, conn, destination, dialer)
		} else {
			proxyOutboundTCP(ctx, conn, destination, dialer)
		}
	}, &padding.DefaultPaddingFactory)
	sess.Run()
	sess.Close()
}

// countingConn wraps a proxy Stream to account transferred bytes against a
// user's traffic quota, closing the stream once the quota is exceeded or the
// account expires. Newly opened streams and sessions are rejected once
// either check fails.
type countingConn struct {
	*session.Stream
	state *user.State
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Stream.Read(b)
	if n > 0 && (c.state.AddTraffic(int64(n)) || c.state.IsExpired()) {
		c.Stream.Close()
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Stream.Write(b)
	if n > 0 && (c.state.AddTraffic(int64(n)) || c.state.IsExpired()) {
		c.Stream.Close()
	}
	return n, err
}

// fallback forwards a connection that failed the anytls auth handshake to a
// local plain service (e.g. a real web server), so active probing sees a
// normal response instead of the connection being dropped. If addr is empty,
// the connection is simply closed by the caller's deferred c.Close().
func fallback(ctx context.Context, c net.Conn, addr string) {
	if addr == "" {
		logrus.Debugln("fallback: no -fallback configured, closing", c.RemoteAddr())
		return
	}
	logrus.Debugln("fallback:", c.RemoteAddr(), "->", addr)

	rc, err := net.Dial("tcp", addr)
	if err != nil {
		logrus.Debugln("fallback dial:", err)
		return
	}
	defer rc.Close()

	bufio.CopyConn(ctx, c, rc)
}
