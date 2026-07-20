package main

import (
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
		fallback(ctx, c)
		return
	}
	var hash [32]byte
	copy(hash[:], by)
	userState, ok := s.userManager.LookupByPasswordHash(hash)
	if !ok {
		b.Resize(0, n)
		fallback(ctx, c)
		return
	}
	by, err = b.ReadBytes(2)
	if err != nil {
		b.Resize(0, n)
		fallback(ctx, c)
		return
	}
	paddingLen := binary.BigEndian.Uint16(by)
	if paddingLen > 0 {
		_, err = b.ReadBytes(int(paddingLen))
		if err != nil {
			b.Resize(0, n)
			fallback(ctx, c)
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

		conn := &countingConn{Stream: stream, state: userState}

		if strings.Contains(destination.String(), "udp-over-tcp.arpa") {
			proxyOutboundUoT(ctx, conn, destination)
		} else {
			proxyOutboundTCP(ctx, conn, destination)
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

func fallback(ctx context.Context, c net.Conn) {
	// 暂未实现
	logrus.Debugln("fallback:", c.RemoteAddr())
}
