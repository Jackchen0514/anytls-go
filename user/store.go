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
	traffic_reset_cycle TEXT NOT NULL DEFAULT 'none',
	traffic_reset_at TIMESTAMP,
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
	var resetAt sql.NullTime
	err := row.Scan(&u.ID, &u.Username, &u.Password, &enabled,
		&u.TrafficLimitBytes, &u.TrafficUsedBytes, &u.IPLimit, &u.ConnLimit,
		&u.TrafficResetCycle, &resetAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	u.Enabled = enabled != 0
	if resetAt.Valid {
		u.TrafficResetAt = resetAt.Time
	}
	return &u, nil
}

const userColumns = `id, username, password, enabled, traffic_limit_bytes, traffic_used_bytes, ip_limit, conn_limit, traffic_reset_cycle, traffic_reset_at, created_at, updated_at`

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

	res, err := s.db.Exec(`INSERT INTO users
		(username, password, enabled, traffic_limit_bytes, traffic_used_bytes, ip_limit, conn_limit, traffic_reset_cycle, traffic_reset_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Username, u.Password, boolToInt(u.Enabled), u.TrafficLimitBytes, u.TrafficUsedBytes,
		u.IPLimit, u.ConnLimit, string(u.TrafficResetCycle), nullTime(u.TrafficResetAt), u.CreatedAt, u.UpdatedAt)
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
	res, err := s.db.Exec(`UPDATE users SET
		username = ?, password = ?, enabled = ?, traffic_limit_bytes = ?, traffic_used_bytes = ?,
		ip_limit = ?, conn_limit = ?, traffic_reset_cycle = ?, traffic_reset_at = ?, updated_at = ?
		WHERE id = ?`,
		u.Username, u.Password, boolToInt(u.Enabled), u.TrafficLimitBytes, u.TrafficUsedBytes,
		u.IPLimit, u.ConnLimit, string(u.TrafficResetCycle), nullTime(u.TrafficResetAt), u.UpdatedAt, u.ID)
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
