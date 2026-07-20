package user

import "time"

// ResetCycle controls how often a user's traffic counter is reset.
type ResetCycle string

const (
	ResetCycleNone    ResetCycle = "none"
	ResetCycleDaily   ResetCycle = "daily"
	ResetCycleMonthly ResetCycle = "monthly"
)

// User is a single anytls account. Zero-value limits mean "unlimited".
type User struct {
	ID                int64      `json:"id"`
	Username          string     `json:"username"`
	Password          string     `json:"password"`
	Enabled           bool       `json:"enabled"`
	TrafficLimitBytes int64      `json:"traffic_limit_bytes"`
	TrafficUsedBytes  int64      `json:"traffic_used_bytes"`
	IPLimit           int        `json:"ip_limit"`
	ConnLimit         int        `json:"conn_limit"`
	TrafficResetCycle ResetCycle `json:"traffic_reset_cycle"`
	TrafficResetAt    time.Time  `json:"traffic_reset_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

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
