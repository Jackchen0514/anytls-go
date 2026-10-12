package proxy

import (
	"fmt"
	"net"
	"time"

	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
)

// baseOutboundDialer is the direct, non-chained dialer - used both as
// OutboundDialer's default and as the transport SetChainSOCKS5 uses to
// reach the upstream chain server itself.
var baseOutboundDialer N.Dialer = &N.DefaultDialer{Dialer: net.Dialer{Timeout: 5 * time.Second}}

// OutboundDialer performs every outbound TCP/UDP connection anytls-server
// makes to a client's requested destination. It defaults to dialing
// directly; call SetChainSOCKS5 to relay all of it through an upstream
// SOCKS5 server instead (at startup via -chain-socks5, or later via the
// admin API/web panel - safe to swap at any time, read fresh on every new
// connection).
var OutboundDialer N.Dialer = baseOutboundDialer

// SetChainSOCKS5 reconfigures OutboundDialer to relay every outbound
// connection through a SOCKS5 server. rawURL is either a bare "host:port"
// (no auth) or a "socks5://user:pass@host:port" URL.
func SetChainSOCKS5(rawURL string) error {
	client, err := socks.NewClientFromURL(baseOutboundDialer, rawURL)
	if err != nil {
		return fmt.Errorf("invalid chain-socks5 url: %w", err)
	}
	OutboundDialer = client
	return nil
}

// ClearChainSOCKS5 reverts OutboundDialer to dialing destinations directly.
func ClearChainSOCKS5() {
	OutboundDialer = baseOutboundDialer
}
