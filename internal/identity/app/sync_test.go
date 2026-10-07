package app

import (
	"context"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/id"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func TestUserSync(t *testing.T) {
	store := newMemStore()
	c := clock.NewFake(t0)
	s := &UserSync{Users: store, Clock: c, IDs: &id.UUIDv7{Clock: c}}
	ctx := context.Background()
	cfg := []domain.ConfiguredUser{{Username: "alice", PasswordHash: "h1"}, {Username: "bob", PasswordHash: "h2"}}

	res, err := s.Sync(ctx, cfg)
	if err != nil || res.Created != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if res, _ := s.Sync(ctx, cfg); res != (SyncResult{}) {
		t.Fatalf("not idempotent: %+v", res)
	}
	bob, _ := store.ByUsername(ctx, "bob")
	sess, _, _ := domain.NewSession(bob.ID, t0, domain.SessionPolicy{IdleTimeout: time.Hour, AbsoluteTimeout: time.Hour}, nil)
	store.Create(ctx, sess)

	res, err = s.Sync(ctx, cfg[:1])
	if err != nil || res.Updated != 1 || res.Revoked != 1 || store.sessionCount() != 0 {
		t.Fatalf("removal: %+v %v, %d sessions", res, err, store.sessionCount())
	}
	bob, _ = store.ByUsername(ctx, "bob")
	if !bob.Disabled {
		t.Fatal("removed user still enabled")
	}
}
