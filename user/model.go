package user

import (
	"net/netip"
	"strings"
	"time"
)

// ResetCycle controls how often a user's traffic counter is reset.
type ResetCycle string

const (
	ResetCycleNone    ResetCycle = "none"
	ResetCycleDaily   ResetCycle = "daily"
	ResetCycleMonthly ResetCycle = "monthly"
)

// User is a single anytls account. Zero-value limits mean "unlimited".
type User struct {
	ID                int64  `json:"id"`
	Username          string `json:"username"`
	Password          string `json:"password"`
	Enabled           bool   `json:"enabled"`
	TrafficLimitBytes int64  `json:"traffic_limit_bytes"`
	TrafficUsedBytes  int64  `json:"traffic_used_bytes"`
	// TrafficMultiplier scales raw transferred bytes before they count against
	// TrafficUsedBytes/TrafficLimitBytes (e.g. 0.9 discounts usage, 2.0
	// doubles it). Zero/negative is treated as 1.0 (no adjustment).
	TrafficMultiplier float64    `json:"traffic_multiplier"`
	IPLimit           int        `json:"ip_limit"`
	ConnLimit         int        `json:"conn_limit"`
	TrafficResetCycle ResetCycle `json:"traffic_reset_cycle"`
	TrafficResetAt    time.Time  `json:"traffic_reset_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	// BlockedDomains holds this user's blocklist: each entry is a domain
	// (example.com), a wildcard domain (*.example.com), an exact IP
	// (203.0.113.1), or a CIDR range (10.0.0.0/8). Populated from a separate
	// table (see Store), not a column on this row - it's nil unless the
	// caller explicitly loaded it (Manager.GetUser/ListUsers do;
	// Store.GetUser/ListUsers alone do not).
	BlockedDomains []string `json:"blocked_domains,omitempty"`
	// OutboundGroupID selects which outbound this user's traffic dials
	// through: DirectGroupID (0, the built-in "direct" group - always
	// valid, never stored as a row) or the ID of an OutboundGroup.
	OutboundGroupID int64 `json:"outbound_group_id"`
}

// DirectGroupID is the built-in outbound group every user defaults to:
// dial destinations directly, no chain proxy. Unlike other groups it's
// never stored as an OutboundGroup row and can't be deleted.
const DirectGroupID int64 = 0

// OutboundGroup is a named outbound path proxied connections can be routed
// through, other than dialing directly: currently always a SOCKS5 server.
// Users are assigned to one via User.OutboundGroupID.
type OutboundGroup struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	SOCKS5URL string    `json:"socks5_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// VisitRecord is one deduplicated entry in a user's recent visit history:
// the most recent time a given destination host (domain or IP) was proxied
// to, and the client IP that made that most recent request (so e.g. a
// multi-device account can be traced back to which device visited what).
type VisitRecord struct {
	Host        string    `json:"host"`
	ClientIP    string    `json:"client_ip"`
	LastVisitAt time.Time `json:"last_visit_at"`
}

// MaxVisitHistory caps how many distinct hosts are kept per user - oldest
// (by last visit) are dropped once this is exceeded.
const MaxVisitHistory = 50

// Usage is a point-in-time snapshot of a user's live resource consumption.
type Usage struct {
	UserID            int64 `json:"user_id"`
	TrafficUsedBytes  int64 `json:"traffic_used_bytes"`
	TrafficLimitBytes int64 `json:"traffic_limit_bytes"`
	ActiveIPs         int   `json:"active_ips"`
	IPLimit           int   `json:"ip_limit"`
	ActiveConns       int   `json:"active_conns"`
	ConnLimit         int   `json:"conn_limit"`
}

// NormalizeBlockPattern lowercases a blocklist pattern and strips a trailing
// dot, so "Example.com." and "example.com" (or "2001:DB8::1" and
// "2001:db8::1") are stored/matched the same way.
func NormalizeBlockPattern(p string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(p), "."))
}

// ValidBlockPattern reports whether p (already normalized) is an acceptable
// blocklist entry: a plain domain ("example.com"), a single leading
// wildcard label ("*.example.com", matching any subdomain but not
// example.com itself), an exact IP ("203.0.113.1"/"2001:db8::1"), or a CIDR
// range ("10.0.0.0/8"/"2001:db8::/32").
func ValidBlockPattern(p string) bool {
	if _, err := netip.ParseAddr(p); err == nil {
		return true
	}
	if _, err := netip.ParsePrefix(p); err == nil {
		return true
	}
	return validDomainShape(p)
}

func validDomainShape(p string) bool {
	base := strings.TrimPrefix(p, "*.")
	if base == p && strings.Contains(p, "*") {
		return false // "*" anywhere but as a "*." prefix
	}
	if base == "" || !strings.Contains(base, ".") {
		return false
	}
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// matchesDomainPattern reports whether host matches pattern. A plain pattern
// like "example.com" matches only that exact host; a pattern prefixed with
// "*." (e.g. "*.example.com") matches any subdomain of example.com, but not
// example.com itself. Both host and pattern are normalized before comparing.
// An IP/CIDR pattern never matches here (it can't equal or suffix-match a
// domain name) - see matchesIPPattern for those.
func matchesDomainPattern(host, pattern string) bool {
	host = NormalizeBlockPattern(host)
	pattern = NormalizeBlockPattern(pattern)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return host == pattern
}

// matchesIPPattern reports whether addr matches pattern, which may be an
// exact IP or a CIDR range. A non-IP/CIDR pattern (a domain/wildcard entry)
// never matches here.
func matchesIPPattern(addr netip.Addr, pattern string) bool {
	pattern = NormalizeBlockPattern(pattern)
	addr = addr.Unmap()
	if prefix, err := netip.ParsePrefix(pattern); err == nil {
		return prefix.Contains(addr)
	}
	if ip, err := netip.ParseAddr(pattern); err == nil {
		return ip.Unmap() == addr
	}
	return false
}

func nextResetAt(from time.Time, cycle ResetCycle) time.Time {
	switch cycle {
	case ResetCycleDaily:
		return from.AddDate(0, 0, 1)
	case ResetCycleMonthly:
		return from.AddDate(0, 1, 0)
	default:
		return time.Time{}
	}
}
