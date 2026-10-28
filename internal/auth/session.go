package auth

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// ErrUnauthorized means the bearer token is missing, unknown, or expired.
var ErrUnauthorized = errors.New("unauthorized")

// ErrNotAllowlisted means the identity verified but is not on the allowlist.
var ErrNotAllowlisted = errors.New("not allowlisted")

// ErrInvalidToken means the identity token could not be verified.
var ErrInvalidToken = errors.New("invalid identity token")

// ErrProviderDisabled means that sign-in provider is not configured.
var ErrProviderDisabled = errors.New("provider not configured")

// Sessions stores opaque bearer tokens in SQLite.
type Sessions struct {
	db  *sql.DB
	now func() time.Time
}

// NewSessions binds session rows to an open inventory database.
func NewSessions(db *sql.DB) *Sessions {
	return &Sessions{db: db, now: time.Now}
}

const sessionSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	token         TEXT PRIMARY KEY,
	user_id       TEXT NOT NULL,
	expires_at_ms INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expires ON sessions(expires_at_ms);
`

// EnsureSchema creates the sessions table when missing.
func (s *Sessions) EnsureSchema() error {
	_, err := s.db.Exec(sessionSchema)
	return err
}

// Create stores a new bearer token for userID. Lifetime is 30 days.
func (s *Sessions) Create(userID string) (token string, expiresAt time.Time, err error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	token = hex.EncodeToString(raw[:])
	expiresAt = s.now().Add(30 * 24 * time.Hour)
	_, err = s.db.Exec(
		`INSERT INTO sessions (token, user_id, expires_at_ms) VALUES (?, ?, ?)`,
		token, userID, expiresAt.UnixMilli(),
	)
	return token, expiresAt, err
}

// Lookup returns the user id for a valid bearer token.
func (s *Sessions) Lookup(token string) (string, error) {
	if token == "" {
		return "", ErrUnauthorized
	}
	var userID string
	var expires int64
	err := s.db.QueryRow(
		`SELECT user_id, expires_at_ms FROM sessions WHERE token = ?`,
		token,
	).Scan(&userID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	if expires <= s.now().UnixMilli() {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
		return "", ErrUnauthorized
	}
	return userID, nil
}

// Delete removes one session. Unknown tokens are ignored.
func (s *Sessions) Delete(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}
