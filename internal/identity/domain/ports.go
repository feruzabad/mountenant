package domain

import (
	"context"
	"errors"
	"time"
)

// Repository errors.
var (
	ErrUserNotFound    = errors.New("user not found")
	ErrSessionNotFound = errors.New("session not found")
)

// UserRepository persists users (spec §4.5).
type UserRepository interface {
	ByUsername(ctx context.Context, name Username) (User, error)
	ByID(ctx context.Context, id UserID) (User, error)
	All(ctx context.Context) ([]User, error)
	// ApplySync applies a SyncPlan atomically, including session revocation.
	ApplySync(ctx context.Context, plan SyncPlan) error
}

// SessionRepository persists sessions (spec §4.5).
type SessionRepository interface {
	Create(ctx context.Context, s Session) error
	Get(ctx context.Context, id SessionID) (Session, error)
	Touch(ctx context.Context, id SessionID, at time.Time) error
	Delete(ctx context.Context, id SessionID) error
	// DeleteInactive removes sessions past their absolute expiry or idle
	// since before idleCutoff, and returns how many were removed.
	DeleteInactive(ctx context.Context, now, idleCutoff time.Time) (int64, error)
}
