package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"time"
)

// tokenBytes is the size of session tokens and CSRF secrets (256 bits).
const tokenBytes = 32

// SessionID is the SHA-256 of the session token. Only the hash is stored;
// the raw token exists only in the cookie (spec §4.1).
type SessionID [sha256.Size]byte

// Session is an authenticated browser session.
type Session struct {
	ID         SessionID
	UserID     UserID
	CSRFSecret []byte
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time // absolute end of life
}

// SessionPolicy holds the session lifetimes from config (spec §7.2).
type SessionPolicy struct {
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
}

var b64 = base64.RawURLEncoding

// NewSession creates a session for userID and returns it with the raw token
// for the cookie. rnd is crypto/rand.Reader outside tests.
func NewSession(userID UserID, now time.Time, p SessionPolicy, rnd io.Reader) (Session, string, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	buf := make([]byte, 2*tokenBytes)
	if _, err := io.ReadFull(rnd, buf); err != nil {
		return Session{}, "", err
	}
	token := b64.EncodeToString(buf[:tokenBytes])
	return Session{
		ID:         sha256.Sum256(buf[:tokenBytes]),
		UserID:     userID,
		CSRFSecret: buf[tokenBytes:],
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(p.AbsoluteTimeout),
	}, token, nil
}

// ErrMalformedToken is returned by ParseToken.
var ErrMalformedToken = errors.New("malformed session token")

// ParseToken checks the token's form and returns the session ID it maps to.
func ParseToken(token string) (SessionID, error) {
	if len(token) != b64.EncodedLen(tokenBytes) {
		return SessionID{}, ErrMalformedToken
	}
	raw, err := b64.DecodeString(token)
	if err != nil {
		return SessionID{}, ErrMalformedToken
	}
	return sha256.Sum256(raw), nil
}

// Active reports whether the session may authenticate at now: before its
// absolute expiry and within the idle timeout of its last use.
func (s Session) Active(now time.Time, p SessionPolicy) bool {
	return now.Before(s.ExpiresAt) && now.Sub(s.LastSeenAt) < p.IdleTimeout
}

// CSRFToken is the synchronizer token for this session (spec §7.3).
func (s Session) CSRFToken() string { return b64.EncodeToString(s.CSRFSecret) }

// CheckCSRF compares a presented token with the session's in constant time.
func (s Session) CheckCSRF(presented string) bool {
	want := s.CSRFToken()
	return len(s.CSRFSecret) == tokenBytes && subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1
}
