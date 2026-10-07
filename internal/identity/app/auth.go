package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/logging"
	"github.com/feruzabad/mountenant/internal/platform/ratelimit"
)

// Errors returned by Auth. Handlers map them to Problem Details.
var (
	// ErrInvalidCredentials covers unknown users, wrong passwords and
	// disabled users alike, so responses do not enumerate users.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrUnauthenticated means no valid session.
	ErrUnauthenticated = errors.New("unauthenticated")
)

// RateLimitedError is returned when a login is throttled.
type RateLimitedError struct{ RetryAfter time.Duration }

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("rate limited, retry after %s", e.RetryAfter)
}

// SecurityLog receives security events (spec §7.8).
type SecurityLog interface {
	Log(logging.SecurityEvent) error
}

// touchEvery limits session last-seen writes to one per interval.
const touchEvery = time.Minute

// maxUsernameKey bounds the rate-limit key built from client input.
const maxUsernameKey = 64

// Auth implements UC-01 (log in), UC-02 (log out) and session
// authentication for every request.
type Auth struct {
	Users    domain.UserRepository
	Sessions domain.SessionRepository
	Hasher   domain.PasswordHasher
	Clock    clock.Clock
	Policy   domain.SessionPolicy
	Security SecurityLog
	Logger   *slog.Logger

	// IPLimit and UserLimit throttle attempts per client IP and per
	// username; UserBackoff adds exponential delays after failures.
	IPLimit     *ratelimit.Buckets
	UserLimit   *ratelimit.Buckets
	UserBackoff *ratelimit.Backoff

	hashes    chan struct{}
	hashWait  time.Duration
	dummyHash string
}

// NewAuth finishes an Auth: it creates the hashing semaphore (spec §7.1:
// at most maxConcurrentHashes argon2id computations, waiting at most
// hashWait) and computes the dummy hash that unknown usernames are verified
// against, with the same cost as real hashes.
func NewAuth(a Auth, maxConcurrentHashes int, hashWait time.Duration) (*Auth, error) {
	pw := make([]byte, 32)
	if _, err := rand.Read(pw); err != nil {
		return nil, err
	}
	dummy, err := a.Hasher.Hash(base64.RawStdEncoding.EncodeToString(pw))
	if err != nil {
		return nil, err
	}
	a.hashes = make(chan struct{}, maxConcurrentHashes)
	a.hashWait = hashWait
	a.dummyHash = dummy
	return &a, nil
}

// LoginInput is one login attempt.
type LoginInput struct {
	Username string
	Password string
	IP       netip.Addr
	// PreviousToken is the session cookie the browser sent, if any. It is
	// revoked on success (rotation, spec §7.2).
	PreviousToken string
}

// LoginResult is a successful login.
type LoginResult struct {
	User    domain.User
	Session domain.Session
	Token   string // raw token for the cookie
}

// Login authenticates a user and opens a new session.
func (a *Auth) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	userKey := strings.ToLower(in.Username)
	if len(userKey) > maxUsernameKey {
		userKey = userKey[:maxUsernameKey]
	}
	event := logging.SecurityEvent{IP: in.IP, User: in.Username}

	// Throttling runs before any hashing (spec §7.1).
	if err := a.throttle(in.IP, userKey); err != nil {
		event.Name, event.Reason = logging.EventAuthRateLimited, "throttled"
		a.audit(event)
		return LoginResult{}, err
	}

	var user domain.User
	known := false
	if name, err := domain.ParseUsername(in.Username); err == nil {
		switch u, err := a.Users.ByUsername(ctx, name); {
		case err == nil:
			user, known = u, true
		case !errors.Is(err, domain.ErrUserNotFound):
			return LoginResult{}, err
		}
	}
	hash := a.dummyHash
	if known {
		hash = user.PasswordHash
	}

	ok, err := a.verify(ctx, in.Password, hash)
	if err != nil {
		var rl *RateLimitedError
		if errors.As(err, &rl) {
			event.Name, event.Reason = logging.EventAuthRateLimited, "hash_busy"
			a.audit(event)
		}
		return LoginResult{}, err
	}
	if !ok || !known || !user.CanAuthenticate() {
		a.UserBackoff.Fail(userKey)
		event.Name, event.Reason = logging.EventAuthFailure, "bad_credentials"
		a.audit(event)
		return LoginResult{}, ErrInvalidCredentials
	}
	a.UserBackoff.Succeed(userKey)

	if id, err := domain.ParseToken(in.PreviousToken); err == nil {
		if err := a.Sessions.Delete(ctx, id); err != nil {
			return LoginResult{}, err
		}
	}
	sess, token, err := domain.NewSession(user.ID, a.Clock.Now(), a.Policy, nil)
	if err != nil {
		return LoginResult{}, err
	}
	if err := a.Sessions.Create(ctx, sess); err != nil {
		return LoginResult{}, err
	}
	event.Name, event.User, event.Reason = logging.EventAuthSuccess, string(user.Username), "ok"
	a.audit(event)
	return LoginResult{User: user, Session: sess, Token: token}, nil
}

func (a *Auth) throttle(ip netip.Addr, userKey string) error {
	ipKey := "-"
	if ip.IsValid() {
		ipKey = ip.Unmap().String()
	}
	var wait time.Duration
	if ok, w := a.IPLimit.Allow(ipKey); !ok {
		wait = max(wait, w)
	}
	if ok, w := a.UserLimit.Allow(userKey); !ok {
		wait = max(wait, w)
	}
	if blocked, w := a.UserBackoff.Blocked(userKey); blocked {
		wait = max(wait, w)
	}
	if wait > 0 {
		return &RateLimitedError{RetryAfter: wait}
	}
	return nil
}

// verify runs one hash comparison under the semaphore.
func (a *Auth) verify(ctx context.Context, password, hash string) (bool, error) {
	timer := time.NewTimer(a.hashWait)
	defer timer.Stop()
	select {
	case a.hashes <- struct{}{}:
	case <-timer.C:
		return false, &RateLimitedError{RetryAfter: time.Second}
	case <-ctx.Done():
		return false, ctx.Err()
	}
	defer func() { <-a.hashes }()
	ok, err := a.Hasher.Verify(password, hash)
	if errors.Is(err, domain.ErrMalformedHash) {
		// Config validation rejects malformed hashes, so this is a bug or a
		// manual database edit; it must not lock the user out silently.
		a.Logger.Error("stored password hash is malformed", "err", err)
		return false, nil
	}
	return ok, err
}

func (a *Auth) audit(e logging.SecurityEvent) {
	if err := a.Security.Log(e); err != nil {
		a.Logger.Error("security log write failed", "err", err)
	}
}

// Authenticate resolves a session token to its user. Expired sessions and
// sessions of disabled users are removed and rejected.
func (a *Auth) Authenticate(ctx context.Context, token string) (domain.User, domain.Session, error) {
	id, err := domain.ParseToken(token)
	if err != nil {
		return domain.User{}, domain.Session{}, ErrUnauthenticated
	}
	sess, err := a.Sessions.Get(ctx, id)
	if errors.Is(err, domain.ErrSessionNotFound) {
		return domain.User{}, domain.Session{}, ErrUnauthenticated
	}
	if err != nil {
		return domain.User{}, domain.Session{}, err
	}
	now := a.Clock.Now()
	if !sess.Active(now, a.Policy) {
		_ = a.Sessions.Delete(ctx, id)
		return domain.User{}, domain.Session{}, ErrUnauthenticated
	}
	user, err := a.Users.ByID(ctx, sess.UserID)
	if errors.Is(err, domain.ErrUserNotFound) || err == nil && !user.CanAuthenticate() {
		_ = a.Sessions.Delete(ctx, id)
		return domain.User{}, domain.Session{}, ErrUnauthenticated
	}
	if err != nil {
		return domain.User{}, domain.Session{}, err
	}
	if now.Sub(sess.LastSeenAt) >= touchEvery {
		if err := a.Sessions.Touch(ctx, id, now); err != nil {
			return domain.User{}, domain.Session{}, err
		}
		sess.LastSeenAt = now
	}
	return user, sess, nil
}

// Logout revokes the session behind token. Unknown tokens are not an error.
func (a *Auth) Logout(ctx context.Context, token string) error {
	if id, err := domain.ParseToken(token); err == nil {
		return a.Sessions.Delete(ctx, id)
	}
	return nil // a malformed token names no session
}

// PruneSessions deletes expired and idle sessions; run periodically.
func (a *Auth) PruneSessions(ctx context.Context) (int64, error) {
	now := a.Clock.Now()
	return a.Sessions.DeleteInactive(ctx, now, now.Add(-a.Policy.IdleTimeout))
}
