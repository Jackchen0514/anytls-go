package proxy

import (
	"fmt"
	"net"
	"sync"
	"time"

	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/protocol/socks"
)

// baseOutboundDialer is the direct, non-chained dialer - used both as the
// fallback for group 0 ("direct") and as the transport SetGroupSOCKS5 uses
// to reach an upstream chain server itself.
var baseOutboundDialer N.Dialer = &N.DefaultDialer{Dialer: net.Dialer{Timeout: 5 * time.Second}}

// DirectGroupID mirrors user.DirectGroupID (this package can't import
// "anytls/user" without an import cycle, since user-facing group CRUD
// calls back into this package). DialerForGroup(DirectGroupID) always
// means "dial directly," regardless of what's registered for other IDs.
const DirectGroupID int64 = 0

var (
	groupsMu sync.RWMutex
	// groups holds only non-direct outbound groups' live dialers, keyed by
	// OutboundGroup.ID. DirectGroupID is intentionally never a key here -
	// it's resolved to baseOutboundDialer directly in DialerForGroup.
	groups = map[int64]N.Dialer{}
)

// DialerForGroup returns the dialer anytls-server should use to reach a
// destination on behalf of a user assigned to groupID: dials directly for
// DirectGroupID, or for any ID with no registered dialer (e.g. the group
// was deleted out from under a user whose record hasn't been reloaded yet).
// Safe to call concurrently with SetGroupSOCKS5/RemoveGroup from any
// goroutine, and reads the latest registration on every call (no caching).
func DialerForGroup(groupID int64) N.Dialer {
	if groupID == DirectGroupID {
		return baseOutboundDialer
	}
	groupsMu.RLock()
	defer groupsMu.RUnlock()
	if d, ok := groups[groupID]; ok {
		return d
	}
	return baseOutboundDialer
}

// SetGroupSOCKS5 (re)configures groupID to relay outbound connections
// through a SOCKS5 server. rawURL is either a bare "host:port" (no auth) or
// a "socks5://user:pass@host:port" URL. Takes effect immediately for any
// connection dialed after this call returns.
func SetGroupSOCKS5(groupID int64, rawURL string) error {
	if groupID == DirectGroupID {
		return fmt.Errorf("group %d is the built-in direct group and can't be reconfigured", groupID)
	}
	client, err := socks.NewClientFromURL(baseOutboundDialer, rawURL)
	if err != nil {
		return fmt.Errorf("invalid socks5 url: %w", err)
	}
	groupsMu.Lock()
	groups[groupID] = client
	groupsMu.Unlock()
	return nil
}

// RemoveGroup forgets a group's live dialer (e.g. after it's deleted from
// the DB), so DialerForGroup falls back to direct dialing for it.
func RemoveGroup(groupID int64) {
	groupsMu.Lock()
	delete(groups, groupID)
	groupsMu.Unlock()
}
