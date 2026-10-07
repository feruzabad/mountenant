// Package app holds the Identity use cases (spec §5.1). Handlers orchestrate
// domain objects and ports; the rules live in the domain.
package app

import (
	"context"
	"fmt"

	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/id"
)

// SyncResult summarises one user sync for the log.
type SyncResult struct {
	Created, Updated, Revoked int
}

// UserSync implements UC-04: mirror the configured users into the database.
// It runs at startup and on SIGHUP and is idempotent.
type UserSync struct {
	Users domain.UserRepository
	Clock clock.Clock
	IDs   id.Generator
}

func (s *UserSync) Sync(ctx context.Context, configured []domain.ConfiguredUser) (SyncResult, error) {
	stored, err := s.Users.All(ctx)
	if err != nil {
		return SyncResult{}, fmt.Errorf("user sync: %w", err)
	}
	plan, err := domain.PlanSync(stored, configured, s.Clock.Now(), func() domain.UserID { return domain.UserID(s.IDs.New()) })
	if err != nil {
		return SyncResult{}, fmt.Errorf("user sync: %w", err)
	}
	if plan.Empty() {
		return SyncResult{}, nil
	}
	if err := s.Users.ApplySync(ctx, plan); err != nil {
		return SyncResult{}, fmt.Errorf("user sync: %w", err)
	}
	return SyncResult{Created: len(plan.Create), Updated: len(plan.Update), Revoked: len(plan.RevokeSessions)}, nil
}
