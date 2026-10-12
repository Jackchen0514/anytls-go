package user

import (
	"crypto/sha256"
	"math"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const flushInterval = 10 * time.Second
const resetCheckInterval = time.Minute

// state is the live, in-process view of a user: identity/limits mirrored from
// the DB plus counters that only ever live in memory between flushes.
type State struct {
	mu sync.Mutex

	user *User // identity + limits, refreshed on CRUD; TrafficUsedBytes is the last-known-flushed value

	usedDelta   int64 // bytes transferred since the last DB flush, not yet added to user.TrafficUsedBytes
	activeIPs   map[string]int
	activeConns int

	visits      []VisitRecord // most-recent-first, deduplicated, capped at MaxVisitHistory
	visitsDirty bool          // true if visits changed since the last DB flush
}

func (st *State) Usage() Usage {
	st.mu.Lock()
	defer st.mu.Unlock()
	return Usage{
		UserID:            st.user.ID,
		TrafficUsedBytes:  st.user.TrafficUsedBytes + st.usedDelta,
		TrafficLimitBytes: st.user.TrafficLimitBytes,
		ActiveIPs:         len(st.activeIPs),
		IPLimit:           st.user.IPLimit,
		ActiveConns:       st.activeConns,
		ConnLimit:         st.user.ConnLimit,
	}
}

// Manager keeps an in-memory index of users for fast per-connection auth and
// limit checks, backed by Store for persistence.
type Manager struct {
	store *Store

	mu        sync.RWMutex
	byID      map[int64]*State
	byPWHash  map[[32]byte]*State
	closeOnce sync.Once
	stop      chan struct{}
}

func NewManager(store *Store) (*Manager, error) {
	m := &Manager{
		store: store,
		byID:  make(map[int64]*State),
		stop:  make(chan struct{}),
	}
	if err := m.reload(); err != nil {
		return nil, err
	}
	go m.loop()
	return m, nil
}

func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		close(m.stop)
		m.flushAll()
	})
}

func pwHash(password string) [32]byte {
	return sha256.Sum256([]byte(password))
}

// reload resyncs the manager's index from the DB. Existing *State objects are
// mutated in place (rather than replaced) so that connections/streams already
// holding a reference see limit/enabled/password changes immediately instead
// of only on their next reconnect.
func (m *Manager) reload() error {
	users, err := m.store.ListUsers()
	if err != nil {
		return err
	}
	blockedDomains, err := m.store.ListAllBlockedDomains()
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	byID := make(map[int64]*State, len(users))
	byPWHash := make(map[[32]byte]*State, len(users))
	for _, u := range users {
		u.BlockedDomains = blockedDomains[u.ID]
		st, exists := m.byID[u.ID]
		if !exists {
			st = &State{activeIPs: make(map[string]int)}
			// Visits live only in memory between flushes (like activeIPs/
			// activeConns), so only seed from the DB once, when the state is
			// first created - not on every reload, which would otherwise
			// stomp on not-yet-flushed in-memory visits with stale DB data.
			if history, err := m.store.ListVisitHistory(u.ID); err != nil {
				logrus.Errorln("[user] load visit history:", err)
			} else {
				if len(history) > MaxVisitHistory {
					history = history[:MaxVisitHistory]
				}
				st.visits = history
			}
		}
		st.mu.Lock()
		st.user = u
		st.mu.Unlock()

		byID[u.ID] = st
		if u.Enabled {
			byPWHash[pwHash(u.Password)] = st
		}
	}
	m.byID = byID
	m.byPWHash = byPWHash
	return nil
}

func (m *Manager) loop() {
	flushTicker := time.NewTicker(flushInterval)
	resetTicker := time.NewTicker(resetCheckInterval)
	defer flushTicker.Stop()
	defer resetTicker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-flushTicker.C:
			m.flushAll()
		case <-resetTicker.C:
			m.checkResets()
		}
	}
}

func (m *Manager) flushAll() {
	m.mu.RLock()
	states := make([]*State, 0, len(m.byID))
	for _, st := range m.byID {
		states = append(states, st)
	}
	m.mu.RUnlock()

	for _, st := range states {
		st.mu.Lock()
		delta := st.usedDelta
		if delta != 0 {
			st.usedDelta = 0
			st.user.TrafficUsedBytes += delta
		}
		var visitsSnapshot []VisitRecord
		if st.visitsDirty {
			visitsSnapshot = append([]VisitRecord(nil), st.visits...)
			st.visitsDirty = false
		}
		id := st.user.ID
		st.mu.Unlock()

		if delta != 0 {
			if err := m.store.AddTrafficUsed(id, delta); err != nil {
				logrus.Errorln("[user] flush traffic:", err)
			}
		}
		if visitsSnapshot != nil {
			if err := m.store.ReplaceVisitHistory(id, visitsSnapshot); err != nil {
				logrus.Errorln("[user] flush visit history:", err)
			}
		}
	}
}

func (m *Manager) checkResets() {
	m.mu.RLock()
	states := make([]*State, 0, len(m.byID))
	for _, st := range m.byID {
		states = append(states, st)
	}
	m.mu.RUnlock()

	now := time.Now()
	for _, st := range states {
		st.mu.Lock()
		due := st.user.TrafficResetCycle != ResetCycleNone && !st.user.TrafficResetAt.IsZero() && now.After(st.user.TrafficResetAt)
		id := st.user.ID
		st.mu.Unlock()
		if due {
			if err := m.store.ResetTraffic(id); err != nil {
				logrus.Errorln("[user] reset traffic:", err)
				continue
			}
			if u, err := m.store.GetUser(id); err == nil {
				st.mu.Lock()
				st.user = u
				st.usedDelta = 0
				st.mu.Unlock()
			}
		}
	}
}

// LookupByPassword returns the enabled user matching this plaintext password, if any.
func (m *Manager) LookupByPasswordHash(hash [32]byte) (*State, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.byPWHash[hash]
	return st, ok
}

// IsOverTraffic reports whether the user has exceeded their traffic quota (0 = unlimited).
func (st *State) IsOverTraffic() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	limit := st.user.TrafficLimitBytes
	if limit <= 0 {
		return false
	}
	return st.user.TrafficUsedBytes+st.usedDelta >= limit
}

// IsExpired reports whether the user's account has passed its expiry time
// (zero ExpiresAt means the account never expires).
func (st *State) IsExpired() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return !st.user.ExpiresAt.IsZero() && !time.Now().Before(st.user.ExpiresAt)
}

// IsDomainBlocked reports whether host matches any of the user's
// blocked-domain patterns (see matchesDomainPattern).
func (st *State) IsDomainBlocked(host string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, p := range st.user.BlockedDomains {
		if matchesDomainPattern(host, p) {
			return true
		}
	}
	return false
}

// RecordVisit notes that host (a domain or IP, deduplicated as-is) was just
// proxied to, moving it to the front of the user's recent-visit list and
// capping the list at MaxVisitHistory. Purely in-memory; Manager's flush
// loop persists it periodically.
func (st *State) RecordVisit(host string) {
	if host == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for i, v := range st.visits {
		if v.Host == host {
			copy(st.visits[1:i+1], st.visits[:i])
			st.visits[0] = VisitRecord{Host: host, LastVisitAt: now}
			st.visitsDirty = true
			return
		}
	}
	st.visits = append([]VisitRecord{{Host: host, LastVisitAt: now}}, st.visits...)
	if len(st.visits) > MaxVisitHistory {
		st.visits = st.visits[:MaxVisitHistory]
	}
	st.visitsDirty = true
}

// VisitHistory returns a copy of the user's recent-visit list, most-recent first.
func (st *State) VisitHistory() []VisitRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]VisitRecord, len(st.visits))
	copy(out, st.visits)
	return out
}

// AcquireIP registers ip as active for this user, enforcing IPLimit (0 = unlimited).
// Returns false if the limit would be exceeded by a new distinct IP.
func (st *State) AcquireIP(ip string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.activeIPs[ip]; ok {
		st.activeIPs[ip]++
		return true
	}
	if st.user.IPLimit > 0 && len(st.activeIPs) >= st.user.IPLimit {
		return false
	}
	st.activeIPs[ip] = 1
	return true
}

func (st *State) ReleaseIP(ip string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if n, ok := st.activeIPs[ip]; ok {
		if n <= 1 {
			delete(st.activeIPs, ip)
		} else {
			st.activeIPs[ip] = n - 1
		}
	}
}

// AcquireConn enforces ConnLimit (0 = unlimited) for a new proxied stream/connection.
func (st *State) AcquireConn() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.user.ConnLimit > 0 && st.activeConns >= st.user.ConnLimit {
		return false
	}
	st.activeConns++
	return true
}

func (st *State) ReleaseConn() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.activeConns > 0 {
		st.activeConns--
	}
}

// AddTraffic accounts n raw bytes against the user, scaled by their traffic
// multiplier, and reports whether they are now over quota.
func (st *State) AddTraffic(n int64) (overQuota bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	mul := st.user.TrafficMultiplier
	if mul <= 0 {
		mul = 1.0
	}
	st.usedDelta += int64(math.Round(float64(n) * mul))
	limit := st.user.TrafficLimitBytes
	if limit <= 0 {
		return false
	}
	return st.user.TrafficUsedBytes+st.usedDelta >= limit
}

func (st *State) Username() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.user.Username
}

func (st *State) ID() int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.user.ID
}

// --- Admin operations (CRUD), delegated to Store and reflected in memory ---

func (m *Manager) ListUsers() ([]*User, error) {
	users, err := m.store.ListUsers()
	if err != nil {
		return nil, err
	}
	blockedDomains, err := m.store.ListAllBlockedDomains()
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		u.BlockedDomains = blockedDomains[u.ID]
	}
	return users, nil
}

func (m *Manager) GetUser(id int64) (*User, error) {
	u, err := m.store.GetUser(id)
	if err != nil {
		return nil, err
	}
	u.BlockedDomains, err = m.store.ListBlockedDomains(id)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (m *Manager) CreateUser(u *User) (*User, error) {
	created, err := m.store.CreateUser(u)
	if err != nil {
		return nil, err
	}
	if err := m.reload(); err != nil {
		return nil, err
	}
	return created, nil
}

func (m *Manager) UpdateUser(u *User) error {
	if err := m.store.UpdateUser(u); err != nil {
		return err
	}
	return m.reload()
}

func (m *Manager) DeleteUser(id int64) error {
	if err := m.store.DeleteUser(id); err != nil {
		return err
	}
	return m.reload()
}

// ListBlockedDomains returns a user's blocked domain/wildcard patterns.
func (m *Manager) ListBlockedDomains(id int64) ([]string, error) {
	if _, err := m.store.GetUser(id); err != nil {
		return nil, err
	}
	return m.store.ListBlockedDomains(id)
}

// AddBlockedDomain adds pattern to a user's blocklist and makes it take
// effect on already-open connections immediately (via reload).
func (m *Manager) AddBlockedDomain(id int64, pattern string) error {
	if _, err := m.store.GetUser(id); err != nil {
		return err
	}
	if err := m.store.AddBlockedDomain(id, pattern); err != nil {
		return err
	}
	return m.reload()
}

// RemoveBlockedDomain removes pattern from a user's blocklist.
func (m *Manager) RemoveBlockedDomain(id int64, pattern string) error {
	if _, err := m.store.GetUser(id); err != nil {
		return err
	}
	if err := m.store.RemoveBlockedDomain(id, pattern); err != nil {
		return err
	}
	return m.reload()
}

func (m *Manager) ResetTraffic(id int64) error {
	if err := m.store.ResetTraffic(id); err != nil {
		return err
	}
	if err := m.reload(); err != nil {
		return err
	}
	// reload() refreshes st.user (traffic_used_bytes now 0 in the DB) but
	// leaves any not-yet-flushed usedDelta in place; clear it too, otherwise
	// pending bytes from before the reset would make the user look
	// over-quota again immediately.
	m.mu.RLock()
	st, ok := m.byID[id]
	m.mu.RUnlock()
	if ok {
		st.mu.Lock()
		st.usedDelta = 0
		st.mu.Unlock()
	}
	return nil
}

// GetUsage returns a live snapshot of a user's usage/limits, including
// in-memory counters not yet flushed to the DB.
func (m *Manager) GetUsage(id int64) (Usage, bool) {
	m.mu.RLock()
	st, ok := m.byID[id]
	m.mu.RUnlock()
	if !ok {
		return Usage{}, false
	}
	return st.Usage(), true
}

// GetVisitHistory returns a live snapshot of a user's recent-visit list
// (most-recent first), including entries not yet flushed to the DB.
func (m *Manager) GetVisitHistory(id int64) ([]VisitRecord, bool) {
	m.mu.RLock()
	st, ok := m.byID[id]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return st.VisitHistory(), true
}

// ClearVisitHistory wipes a user's recorded visit history, immediately
// (unlike per-connection RecordVisit, which only flushes periodically).
func (m *Manager) ClearVisitHistory(id int64) error {
	if _, err := m.store.GetUser(id); err != nil {
		return err
	}
	if err := m.store.ReplaceVisitHistory(id, nil); err != nil {
		return err
	}
	m.mu.RLock()
	st, ok := m.byID[id]
	m.mu.RUnlock()
	if ok {
		st.mu.Lock()
		st.visits = nil
		st.visitsDirty = false
		st.mu.Unlock()
	}
	return nil
}

// UserCount returns the number of users currently loaded (used to decide
// whether to bootstrap a default user from a legacy -p password flag).
func (m *Manager) UserCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}
