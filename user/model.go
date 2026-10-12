package user

import (
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
	// BlockedDomains is populated from a separate table (see Store), not a
	// column on this row - it's nil unless the caller explicitly loaded it
	// (Manager.GetUser/ListUsers do; Store.GetUser/ListUsers alone do not).
	BlockedDomains []string `json:"blocked_domains,omitempty"`
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

// NormalizeDomainPattern lowercases a blocked-domain pattern and strips a
// trailing dot, so "Example.com." and "example.com" are stored/matched the
// same way.
func NormalizeDomainPattern(p string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(p), "."))
}

// ValidDomainPattern reports whether p (already normalized) is an acceptable
// blocked-domain pattern: a plain domain (e.g. "example.com") or a single
// leading wildcard label (e.g. "*.example.com", which matches any subdomain
// but not example.com itself).
func ValidDomainPattern(p string) bool {
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
func matchesDomainPattern(host, pattern string) bool {
	host = NormalizeDomainPattern(host)
	pattern = NormalizeDomainPattern(pattern)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return host == pattern
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
