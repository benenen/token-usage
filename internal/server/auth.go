package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// KeyPrefix is the cosmetic prefix that lets a leaked key be recognized
// at a glance and grepped out of logs.
const KeyPrefix = "tuk_"

// ErrInvalidKey covers both "no such key" and "revoked key" — the caller
// never gets to distinguish; we don't reveal whether a key existed.
var ErrInvalidKey = errors.New("invalid or revoked api key")

// NewAPIKey returns (raw, hash, displayPrefix). `raw` is shown to the user
// exactly once; `hash` is what we store; `displayPrefix` is the first 12
// characters (tuk_ + 8 random) — safe to log and useful for revoke.
func NewAPIKey() (raw, hash, prefix string, err error) {
	buf := make([]byte, 24) // 192 bits of entropy
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	raw = KeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	hash = hashKey(raw)
	prefix = raw[:12]
	return
}

func hashKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// ResolveAPIKey looks up a presented key and returns the owning user_id.
// It also bumps last_used_at — useful for spotting dead keys later. The
// UPDATE … RETURNING does both in one round trip and is atomic.
func (s *Store) ResolveAPIKey(ctx context.Context, raw string) (string, error) {
	if !strings.HasPrefix(raw, KeyPrefix) {
		return "", ErrInvalidKey
	}
	var userID string
	err := s.pool.QueryRow(ctx, `
		UPDATE api_keys SET last_used_at = NOW()
		WHERE key_hash = $1 AND revoked_at IS NULL
		RETURNING user_id
	`, hashKey(raw)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidKey
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

// ---- user / key admin ---------------------------------------------------

type UserRow struct {
	UserID    string
	Email     string
	CreatedAt time.Time
	Disabled  bool
}

func (s *Store) CreateUser(ctx context.Context, userID, email string) error {
	if userID == "" {
		return errors.New("user_id is required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (user_id, email) VALUES ($1, NULLIF($2,''))
	`, userID, email)
	return err
}

func (s *Store) ListUsers(ctx context.Context) ([]UserRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id, COALESCE(email,''), created_at, disabled_at IS NOT NULL
		FROM users ORDER BY user_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var r UserRow
		if err := rows.Scan(&r.UserID, &r.Email, &r.CreatedAt, &r.Disabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateAPIKey mints a new key for userID. Returns the raw key (shown once)
// and the prefix (safe to display). The user must exist.
func (s *Store) CreateAPIKey(ctx context.Context, userID, name string) (raw, prefix string, err error) {
	raw, hash, prefix, err := NewAPIKey()
	if err != nil {
		return "", "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO api_keys (key_hash, key_prefix, user_id, name)
		VALUES ($1, $2, $3, NULLIF($4,''))
	`, hash, prefix, userID, name)
	if err != nil {
		return "", "", err
	}
	return raw, prefix, nil
}

type APIKeyRow struct {
	Prefix     string
	UserID     string
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	Revoked    bool
}

// ListAPIKeys: empty userID returns all keys (admin view).
func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]APIKeyRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT key_prefix, user_id, COALESCE(name,''), created_at, last_used_at, revoked_at IS NOT NULL
		FROM api_keys
		WHERE ($1 = '' OR user_id = $1)
		ORDER BY user_id, created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyRow
	for rows.Next() {
		var r APIKeyRow
		if err := rows.Scan(&r.Prefix, &r.UserID, &r.Name, &r.CreatedAt, &r.LastUsedAt, &r.Revoked); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeAPIKey marks the matching key as revoked. Matches by prefix
// (unique-enough in practice; the caller can pass the full key too —
// we only key on the first 12 chars).
func (s *Store) RevokeAPIKey(ctx context.Context, prefix string) error {
	if len(prefix) >= 12 {
		prefix = prefix[:12]
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE api_keys SET revoked_at = NOW()
		WHERE key_prefix = $1 AND revoked_at IS NULL
	`, prefix)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return errors.New("no matching active key for prefix " + prefix)
	}
	return nil
}
