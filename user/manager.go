package user

import (
	"crypto/sha256"
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

	m.mu.Lock()
	defer m.mu.Unlock()

	byID := make(map[int64]*State, len(users))
	byPWHash := make(map[[32]byte]*State, len(users))
	for _, u := range users {
		st, exists := m.byID[u.ID]
		if !exists {
			st = &State{activeIPs: make(map[string]int)}
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
		id := st.user.ID
		st.mu.Unlock()

		if delta != 0 {
			if err := m.store.AddTrafficUsed(id, delta); err != nil {
				logrus.Errorln("[user] flush traffic:", err)
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

// AddTraffic accounts n bytes against the user and reports whether they are now over quota.
func (st *State) AddTraffic(n int64) (overQuota bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.usedDelta += n
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
	return m.store.ListUsers()
}

func (m *Manager) GetUser(id int64) (*User, error) {
	return m.store.GetUser(id)
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

// UserCount returns the number of users currently loaded (used to decide
// whether to bootstrap a default user from a legacy -p password flag).
func (m *Manager) UserCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}
