package user

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("user: not found")
var ErrDuplicateUsername = errors.New("user: duplicate username")
var ErrGroupNotFound = errors.New("user: outbound group not found")
var ErrDuplicateGroupName = errors.New("user: outbound group name already exists")

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// modernc.org/sqlite connections are not safe for concurrent write access.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	username TEXT NOT NULL UNIQUE,
	password TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	traffic_limit_bytes INTEGER NOT NULL DEFAULT 0,
	traffic_used_bytes INTEGER NOT NULL DEFAULT 0,
	ip_limit INTEGER NOT NULL DEFAULT 0,
	conn_limit INTEGER NOT NULL DEFAULT 0,
	traffic_multiplier REAL NOT NULL DEFAULT 1.0,
	traffic_reset_cycle TEXT NOT NULL DEFAULT 'none',
	traffic_reset_at TIMESTAMP,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
)`); err != nil {
		db.Close()
		return nil, err
	}

	// Migration for databases created before expires_at existed. SQLite has
	// no "ADD COLUMN IF NOT EXISTS" guaranteed across versions, so just
	// attempt it and ignore a "column already exists" failure.
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN expires_at TIMESTAMP`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, err
		}
	}

	// Migration for databases created before traffic_multiplier existed.
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN traffic_multiplier REAL NOT NULL DEFAULT 1.0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, err
		}
	}

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS user_blocked_domains (
	user_id INTEGER NOT NULL,
	pattern TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL,
	PRIMARY KEY (user_id, pattern)
)`); err != nil {
		db.Close()
		return nil, err
	}

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS user_visit_history (
	user_id INTEGER NOT NULL,
	host TEXT NOT NULL,
	client_ip TEXT NOT NULL DEFAULT '',
	last_visit_at TIMESTAMP NOT NULL,
	PRIMARY KEY (user_id, host)
)`); err != nil {
		db.Close()
		return nil, err
	}

	// Migration for databases created before outbound_group_id existed.
	if _, err := db.Exec(`ALTER TABLE users ADD COLUMN outbound_group_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, err
		}
	}

	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS outbound_groups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	socks5_url TEXT NOT NULL,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
)`); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func scanUser(row interface {
	Scan(dest ...any) error
}) (*User, error) {
	var u User
	var enabled int
	var resetAt, expiresAt sql.NullTime
	err := row.Scan(&u.ID, &u.Username, &u.Password, &enabled,
		&u.TrafficLimitBytes, &u.TrafficUsedBytes, &u.IPLimit, &u.ConnLimit, &u.TrafficMultiplier,
		&u.TrafficResetCycle, &resetAt, &expiresAt, &u.CreatedAt, &u.UpdatedAt, &u.OutboundGroupID)
	if err != nil {
		return nil, err
	}
	u.Enabled = enabled != 0
	if resetAt.Valid {
		u.TrafficResetAt = resetAt.Time
	}
	if expiresAt.Valid {
		u.ExpiresAt = expiresAt.Time
	}
	return &u, nil
}

const userColumns = `id, username, password, enabled, traffic_limit_bytes, traffic_used_bytes, ip_limit, conn_limit, traffic_multiplier, traffic_reset_cycle, traffic_reset_at, expires_at, created_at, updated_at, outbound_group_id`

func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userColumns + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) GetUser(id int64) (*User, error) {
	row := s.db.QueryRow(`SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) CreateUser(u *User) (*User, error) {
	now := time.Now()
	u.CreatedAt = now
	u.UpdatedAt = now
	if u.TrafficResetCycle == "" {
		u.TrafficResetCycle = ResetCycleNone
	}
	if u.TrafficResetAt.IsZero() {
		u.TrafficResetAt = nextResetAt(now, u.TrafficResetCycle)
	}
	if u.TrafficMultiplier <= 0 {
		u.TrafficMultiplier = 1.0
	}

	res, err := s.db.Exec(`INSERT INTO users
		(username, password, enabled, traffic_limit_bytes, traffic_used_bytes, ip_limit, conn_limit, traffic_multiplier, traffic_reset_cycle, traffic_reset_at, expires_at, created_at, updated_at, outbound_group_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Username, u.Password, boolToInt(u.Enabled), u.TrafficLimitBytes, u.TrafficUsedBytes,
		u.IPLimit, u.ConnLimit, u.TrafficMultiplier, string(u.TrafficResetCycle), nullTime(u.TrafficResetAt), nullTime(u.ExpiresAt), u.CreatedAt, u.UpdatedAt, u.OutboundGroupID)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return nil, ErrDuplicateUsername
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	u.ID = id
	return u, nil
}

func (s *Store) UpdateUser(u *User) error {
	u.UpdatedAt = time.Now()
	if u.TrafficMultiplier <= 0 {
		u.TrafficMultiplier = 1.0
	}
	res, err := s.db.Exec(`UPDATE users SET
		username = ?, password = ?, enabled = ?, traffic_limit_bytes = ?, traffic_used_bytes = ?,
		ip_limit = ?, conn_limit = ?, traffic_multiplier = ?, traffic_reset_cycle = ?, traffic_reset_at = ?, expires_at = ?, updated_at = ?, outbound_group_id = ?
		WHERE id = ?`,
		u.Username, u.Password, boolToInt(u.Enabled), u.TrafficLimitBytes, u.TrafficUsedBytes,
		u.IPLimit, u.ConnLimit, u.TrafficMultiplier, string(u.TrafficResetCycle), nullTime(u.TrafficResetAt), nullTime(u.ExpiresAt), u.UpdatedAt, u.OutboundGroupID, u.ID)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrDuplicateUsername
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteUser(id int64) error {
	// No foreign key enforcement is set up on user_blocked_domains/
	// user_visit_history, so clean them up explicitly rather than leaving
	// orphaned rows behind.
	if _, err := s.db.Exec(`DELETE FROM user_blocked_domains WHERE user_id = ?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM user_visit_history WHERE user_id = ?`, id); err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListBlockedDomains returns the domain/wildcard patterns blocked for a
// single user, sorted for stable output.
func (s *Store) ListBlockedDomains(userID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT pattern FROM user_blocked_domains WHERE user_id = ? ORDER BY pattern`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var patterns []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		patterns = append(patterns, p)
	}
	return patterns, rows.Err()
}

// ListAllBlockedDomains batch-loads every user's blocked patterns in one
// query, keyed by user ID, for Manager.reload() to avoid an N+1 query per
// reload.
func (s *Store) ListAllBlockedDomains() (map[int64][]string, error) {
	rows, err := s.db.Query(`SELECT user_id, pattern FROM user_blocked_domains ORDER BY user_id, pattern`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64][]string)
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		result[id] = append(result[id], p)
	}
	return result, rows.Err()
}

// AddBlockedDomain records a domain/wildcard pattern to block for a user.
// Idempotent: adding the same pattern twice is a no-op.
func (s *Store) AddBlockedDomain(userID int64, pattern string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO user_blocked_domains (user_id, pattern, created_at) VALUES (?, ?, ?)`,
		userID, pattern, time.Now())
	return err
}

// RemoveBlockedDomain removes a previously-added pattern. Removing a pattern
// that isn't present is a no-op, not an error.
func (s *Store) RemoveBlockedDomain(userID int64, pattern string) error {
	_, err := s.db.Exec(`DELETE FROM user_blocked_domains WHERE user_id = ? AND pattern = ?`, userID, pattern)
	return err
}

// ListVisitHistory returns a user's recorded visit history, most-recent
// first.
func (s *Store) ListVisitHistory(userID int64) ([]VisitRecord, error) {
	rows, err := s.db.Query(`SELECT host, client_ip, last_visit_at FROM user_visit_history WHERE user_id = ? ORDER BY last_visit_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []VisitRecord
	for rows.Next() {
		var v VisitRecord
		if err := rows.Scan(&v.Host, &v.ClientIP, &v.LastVisitAt); err != nil {
			return nil, err
		}
		history = append(history, v)
	}
	return history, rows.Err()
}

// ReplaceVisitHistory atomically overwrites a user's whole visit history
// with entries (expected already deduplicated and capped by the caller).
// Called periodically by Manager's flush loop rather than per-connection, so
// a full delete+reinsert here is simpler than diffing and cheap enough given
// the small capped size.
func (s *Store) ReplaceVisitHistory(userID int64, entries []VisitRecord) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM user_visit_history WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO user_visit_history (user_id, host, client_ip, last_visit_at) VALUES (?, ?, ?, ?)`,
			userID, e.Host, e.ClientIP, e.LastVisitAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AddTrafficUsed atomically adds delta to the stored usage counter.
func (s *Store) AddTrafficUsed(id int64, delta int64) error {
	if delta == 0 {
		return nil
	}
	_, err := s.db.Exec(`UPDATE users SET traffic_used_bytes = traffic_used_bytes + ?, updated_at = ? WHERE id = ?`,
		delta, time.Now(), id)
	return err
}

// ResetTraffic zeroes a user's used-traffic counter and advances the reset window if a cycle is set.
func (s *Store) ResetTraffic(id int64) error {
	u, err := s.GetUser(id)
	if err != nil {
		return err
	}
	now := time.Now()
	u.TrafficUsedBytes = 0
	u.TrafficResetAt = nextResetAt(now, u.TrafficResetCycle)
	return s.UpdateUser(u)
}

func scanOutboundGroup(row interface {
	Scan(dest ...any) error
}) (*OutboundGroup, error) {
	var g OutboundGroup
	err := row.Scan(&g.ID, &g.Name, &g.SOCKS5URL, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

const outboundGroupColumns = `id, name, socks5_url, created_at, updated_at`

// ListOutboundGroups returns every outbound group (not including the
// built-in DirectGroupID, which is never a row).
func (s *Store) ListOutboundGroups() ([]*OutboundGroup, error) {
	rows, err := s.db.Query(`SELECT ` + outboundGroupColumns + ` FROM outbound_groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []*OutboundGroup
	for rows.Next() {
		g, err := scanOutboundGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (s *Store) GetOutboundGroup(id int64) (*OutboundGroup, error) {
	row := s.db.QueryRow(`SELECT `+outboundGroupColumns+` FROM outbound_groups WHERE id = ?`, id)
	g, err := scanOutboundGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGroupNotFound
	}
	return g, err
}

func (s *Store) CreateOutboundGroup(g *OutboundGroup) (*OutboundGroup, error) {
	now := time.Now()
	g.CreatedAt = now
	g.UpdatedAt = now
	res, err := s.db.Exec(`INSERT INTO outbound_groups (name, socks5_url, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		g.Name, g.SOCKS5URL, g.CreatedAt, g.UpdatedAt)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return nil, ErrDuplicateGroupName
		}
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	g.ID = id
	return g, nil
}

func (s *Store) UpdateOutboundGroup(g *OutboundGroup) error {
	g.UpdatedAt = time.Now()
	res, err := s.db.Exec(`UPDATE outbound_groups SET name = ?, socks5_url = ?, updated_at = ? WHERE id = ?`,
		g.Name, g.SOCKS5URL, g.UpdatedAt, g.ID)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrDuplicateGroupName
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrGroupNotFound
	}
	return nil
}

// DeleteOutboundGroup removes a group and resets any user assigned to it
// back to DirectGroupID, so no user is left pointing at a nonexistent group.
func (s *Store) DeleteOutboundGroup(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE users SET outbound_group_id = 0 WHERE outbound_group_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM outbound_groups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrGroupNotFound
	}
	return tx.Commit()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed: UNIQUE")
}
