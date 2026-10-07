package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/logging"
	"github.com/feruzabad/mountenant/internal/platform/ratelimit"
)

// fakeHasher stores "h:" + password; verifications are counted and can be
// held to test the semaphore.
type fakeHasher struct {
	verifies atomic.Int32
	hold     chan struct{}
}

func (f *fakeHasher) Hash(pw string) (string, error) { return "h:" + pw, nil }

func (f *fakeHasher) Verify(pw, enc string) (bool, error) {
	f.verifies.Add(1)
	if f.hold != nil {
		<-f.hold
	}
	if !strings.HasPrefix(enc, "h:") {
		return false, domain.ErrMalformedHash
	}
	return enc == "h:"+pw, nil
}

type events struct {
	mu   sync.Mutex
	list []logging.SecurityEvent
}

func (e *events) Log(ev logging.SecurityEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
	return nil
}

func (e *events) last() logging.SecurityEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.list[len(e.list)-1]
}

type fixture struct {
	auth   *Auth
	store  *memStore
	hasher *fakeHasher
	events *events
	clock  *clock.Fake
	alice  domain.User
}

var (
	clientIP = netip.MustParseAddr("203.0.113.9")
	policy   = domain.SessionPolicy{IdleTimeout: time.Hour, AbsoluteTimeout: 24 * time.Hour}
)

func newFixture(t *testing.T, maxHashes int) *fixture {
	t.Helper()
	f := &fixture{store: newMemStore(), hasher: &fakeHasher{}, events: &events{}, clock: clock.NewFake(t0)}
	f.alice = domain.User{ID: "u1", Username: "alice", PasswordHash: "h:secret password"}
	f.store.ApplySync(context.Background(), domain.SyncPlan{Create: []domain.User{
		f.alice,
		{ID: "u2", Username: "bob", PasswordHash: "h:bob password", Disabled: true},
	}})
	a, err := NewAuth(Auth{
		Users: f.store, Sessions: f.store, Hasher: f.hasher, Clock: f.clock, Policy: policy,
		Security: f.events, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		IPLimit:     &ratelimit.Buckets{PerMinute: 100, Burst: 100, Now: f.clock.Now},
		UserLimit:   &ratelimit.Buckets{PerMinute: 100, Burst: 100, Now: f.clock.Now},
		UserBackoff: &ratelimit.Backoff{Free: 3, Base: time.Second, Max: time.Minute, Now: f.clock.Now},
	}, maxHashes, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	f.auth = a
	return f
}

func (f *fixture) login(user, pw, prev string) (LoginResult, error) {
	return f.auth.Login(context.Background(), LoginInput{Username: user, Password: pw, IP: clientIP, PreviousToken: prev})
}

func TestLoginSuccessAndRotation(t *testing.T) {
	f := newFixture(t, 2)
	first, err := f.login("Alice", "secret password", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.User.ID != "u1" || first.Token == "" {
		t.Fatalf("%+v", first)
	}
	if ev := f.events.last(); ev.Name != logging.EventAuthSuccess || ev.User != "alice" || ev.IP != clientIP {
		t.Fatalf("event %+v", ev)
	}
	second, err := f.login("alice", "secret password", first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.auth.Authenticate(context.Background(), first.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("previous session survived login (no rotation)")
	}
	if _, _, err := f.auth.Authenticate(context.Background(), second.Token); err != nil {
		t.Fatal(err)
	}
}

func TestLoginFailuresLookAlike(t *testing.T) {
	f := newFixture(t, 2)
	for _, c := range []struct{ user, pw string }{
		{"alice", "wrong"},             // wrong password
		{"mallory", "secret password"}, // unknown user
		{"bob", "bob password"},        // disabled user, right password
		{"x y", "whatever"},            // not even a valid username
	} {
		before := f.hasher.verifies.Load()
		_, err := f.login(c.user, c.pw, "")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: %v", c.user, err)
		}
		if f.hasher.verifies.Load() != before+1 {
			t.Errorf("%s: no hash computed; timing would reveal the user", c.user)
		}
		if ev := f.events.last(); ev.Name != logging.EventAuthFailure || ev.Reason != "bad_credentials" {
			t.Errorf("%s: event %+v", c.user, ev)
		}
	}
}

func TestLoginBackoffAndRateLimit(t *testing.T) {
	f := newFixture(t, 2)
	for i := 0; i < 4; i++ {
		f.login("alice", "wrong", "")
	}
	_, err := f.login("alice", "secret password", "")
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfter != time.Second {
		t.Fatalf("after 4 failures: %v", err)
	}
	if ev := f.events.last(); ev.Name != logging.EventAuthRateLimited {
		t.Fatalf("event %+v", ev)
	}
	verifies := f.hasher.verifies.Load()
	f.login("alice", "secret password", "")
	if f.hasher.verifies.Load() != verifies {
		t.Fatal("hash computed for a throttled attempt")
	}
	f.clock.Advance(time.Second)
	if _, err := f.login("alice", "secret password", ""); err != nil {
		t.Fatalf("after waiting: %v", err)
	}

	// Per-IP bucket.
	f.auth.IPLimit = &ratelimit.Buckets{PerMinute: 1, Burst: 1, Now: f.clock.Now}
	f.login("alice", "secret password", "")
	if _, err := f.login("alice", "secret password", ""); !errors.As(err, &rl) {
		t.Fatalf("IP limit: %v", err)
	}
}

func TestLoginHashSemaphore(t *testing.T) {
	f := newFixture(t, 1)
	f.hasher.hold = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := f.login("alice", "secret password", ""); done <- err }()
	for f.hasher.verifies.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// The only slot is taken: the next attempt waits hashWait, then gives up.
	_, err := f.login("alice", "secret password", "")
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("got %v", err)
	}
	if ev := f.events.last(); ev.Reason != "hash_busy" {
		t.Fatalf("event %+v", ev)
	}
	close(f.hasher.hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticate(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	res, _ := f.login("alice", "secret password", "")

	f.clock.Advance(30 * time.Second)
	_, s, err := f.auth.Authenticate(ctx, res.Token)
	if err != nil || !s.LastSeenAt.Equal(t0) {
		t.Fatalf("within touch interval: %v %v", s.LastSeenAt, err)
	}
	f.clock.Advance(time.Minute)
	if _, s, _ = f.auth.Authenticate(ctx, res.Token); !s.LastSeenAt.Equal(f.clock.Now()) {
		t.Fatal("last seen not updated")
	}

	f.clock.Advance(policy.IdleTimeout)
	if _, _, err := f.auth.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("idle session accepted")
	}
	if f.store.sessionCount() != 0 {
		t.Fatal("expired session not removed")
	}

	for _, tok := range []string{"", "garbage", strings.Repeat("A", 43)} {
		if _, _, err := f.auth.Authenticate(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%q: %v", tok, err)
		}
	}
}

func TestAuthenticateRejectsDisabledUser(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	res, _ := f.login("alice", "secret password", "")
	alice, _ := f.store.ByID(ctx, "u1")
	alice.Disabled = true
	// A direct update without RevokeSessions: Authenticate must still refuse.
	f.store.ApplySync(ctx, domain.SyncPlan{Update: []domain.User{alice}})
	if _, _, err := f.auth.Authenticate(ctx, res.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("disabled user authenticated")
	}
}

func TestLogoutAndPrune(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	a, _ := f.login("alice", "secret password", "")
	f.login("alice", "secret password", "") // a second session, pruned below
	if err := f.auth.Logout(ctx, a.Token); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.auth.Authenticate(ctx, a.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("logged-out session still valid")
	}
	if err := f.auth.Logout(ctx, "garbage"); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Hour)
	if n, err := f.auth.PruneSessions(ctx); n != 1 || err != nil {
		t.Fatalf("prune: %d %v", n, err)
	}
}
