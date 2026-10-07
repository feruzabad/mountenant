package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/db"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 123_000_000, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"), 5000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(ctx, d, nil); err != nil {
		t.Fatal(err)
	}
	return New(d)
}

func TestUsersRoundTripAndSync(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	q := domain.Quota{MaxActiveJobs: 1, MaxTotalJobs: 2, MaxNZBBytes: 3, MaxDownloadBytesPerDay: 4, MaxConcurrentDownloads: 5}
	alice := domain.User{ID: "u1", Username: "alice", PasswordHash: "h", Quota: q, ConfigVersion: "v1", CreatedAt: t0, UpdatedAt: t0}
	if err := s.ApplySync(ctx, domain.SyncPlan{Create: []domain.User{alice}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != alice.ID || got.Quota != q || !got.CreatedAt.Equal(t0) || got.Disabled {
		t.Fatalf("round trip: %+v", got)
	}
	if _, err := s.ByUsername(ctx, "bob"); !errors.Is(err, domain.ErrUserNotFound) {
		t.Fatalf("missing user: %v", err)
	}

	sess, _, _ := domain.NewSession("u1", t0, domain.SessionPolicy{IdleTimeout: time.Hour, AbsoluteTimeout: 24 * time.Hour}, nil)
	if err := s.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	alice.Disabled, alice.ConfigVersion, alice.UpdatedAt = true, "removed", t0.Add(time.Hour)
	if err := s.ApplySync(ctx, domain.SyncPlan{Update: []domain.User{alice}, RevokeSessions: []domain.UserID{"u1"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ByID(ctx, "u1")
	if !got.Disabled || got.ConfigVersion != "removed" {
		t.Fatalf("update: %+v", got)
	}
	if _, err := s.Get(ctx, sess.ID); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Fatalf("session not revoked: %v", err)
	}
	all, _ := s.All(ctx)
	if len(all) != 1 {
		t.Fatalf("all: %d", len(all))
	}
}

func TestApplySyncIsAtomic(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u := domain.User{ID: "u1", Username: "alice", PasswordHash: "h", ConfigVersion: "v", CreatedAt: t0, UpdatedAt: t0}
	dup := u
	dup.ID = "u2" // same username: unique violation on the second insert
	if err := s.ApplySync(ctx, domain.SyncPlan{Create: []domain.User{u, dup}}); err == nil {
		t.Fatal("expected unique violation")
	}
	if all, _ := s.All(ctx); len(all) != 0 {
		t.Fatalf("partial sync committed: %d users", len(all))
	}
}

func TestSessions(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	s.ApplySync(ctx, domain.SyncPlan{Create: []domain.User{{ID: "u1", Username: "alice", PasswordHash: "h", ConfigVersion: "v", CreatedAt: t0, UpdatedAt: t0}}})
	p := domain.SessionPolicy{IdleTimeout: time.Hour, AbsoluteTimeout: 24 * time.Hour}

	fresh, _, _ := domain.NewSession("u1", t0, p, nil)
	idle, _, _ := domain.NewSession("u1", t0.Add(-2*time.Hour), p, nil)
	old, _, _ := domain.NewSession("u1", t0.Add(-25*time.Hour), p, nil)
	for _, x := range []domain.Session{fresh, idle, old} {
		if err := s.Create(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, fresh.ID)
	if err != nil || got.UserID != "u1" || string(got.CSRFSecret) != string(fresh.CSRFSecret) || !got.ExpiresAt.Equal(fresh.ExpiresAt) {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := s.Touch(ctx, fresh.ID, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, fresh.ID)
	if !got.LastSeenAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("touch: %v", got.LastSeenAt)
	}

	n, err := s.DeleteInactive(ctx, t0, t0.Add(-p.IdleTimeout))
	if err != nil || n != 2 {
		t.Fatalf("delete inactive: %d %v", n, err)
	}
	if err := s.Delete(ctx, fresh.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, fresh.ID); !errors.Is(err, domain.ErrSessionNotFound) {
		t.Fatal(err)
	}
}
