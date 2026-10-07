// Package sqlite implements the Identity repositories on SQLite. Queries are
// in queries.sql and compiled by sqlc into package gen.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/feruzabad/mountenant/internal/identity/adapters/sqlite/gen"
	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/db"
)

// Store implements domain.UserRepository and domain.SessionRepository.
// Reads use the read pool, writes the single writer connection.
type Store struct {
	db *db.DB
	r  *gen.Queries
	w  *gen.Queries
}

var (
	_ domain.UserRepository    = (*Store)(nil)
	_ domain.SessionRepository = (*Store)(nil)
)

// New returns a Store on d.
func New(d *db.DB) *Store {
	return &Store{db: d, r: gen.New(d.R), w: gen.New(d.W)}
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }
func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func notFound(err, as error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return as
	}
	return err
}

func toUser(u gen.User) (domain.User, error) {
	var q domain.Quota
	if err := json.Unmarshal([]byte(u.QuotaJson), &q); err != nil {
		return domain.User{}, fmt.Errorf("user %s: quota: %w", u.ID, err)
	}
	return domain.User{
		ID:            domain.UserID(u.ID),
		Username:      domain.Username(u.Username),
		PasswordHash:  u.PasswordHash,
		Quota:         q,
		Disabled:      u.Disabled != 0,
		ConfigVersion: u.ConfigVersion,
		CreatedAt:     fromMs(u.CreatedAt),
		UpdatedAt:     fromMs(u.UpdatedAt),
	}, nil
}

func (s *Store) ByUsername(ctx context.Context, name domain.Username) (domain.User, error) {
	u, err := s.r.UserByUsername(ctx, string(name))
	if err != nil {
		return domain.User{}, notFound(err, domain.ErrUserNotFound)
	}
	return toUser(u)
}

func (s *Store) ByID(ctx context.Context, id domain.UserID) (domain.User, error) {
	u, err := s.r.UserByID(ctx, string(id))
	if err != nil {
		return domain.User{}, notFound(err, domain.ErrUserNotFound)
	}
	return toUser(u)
}

func (s *Store) All(ctx context.Context) ([]domain.User, error) {
	rows, err := s.r.AllUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.User, 0, len(rows))
	for _, r := range rows {
		u, err := toUser(r)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// ApplySync writes the plan in one transaction.
func (s *Store) ApplySync(ctx context.Context, plan domain.SyncPlan) (err error) {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	q := s.w.WithTx(tx)
	for _, u := range plan.Create {
		quota, err := json.Marshal(u.Quota)
		if err != nil {
			return err
		}
		if err := q.InsertUser(ctx, gen.InsertUserParams{
			ID: string(u.ID), Username: string(u.Username), PasswordHash: u.PasswordHash, QuotaJson: string(quota),
			Disabled: boolInt(u.Disabled), ConfigVersion: u.ConfigVersion, CreatedAt: ms(u.CreatedAt), UpdatedAt: ms(u.UpdatedAt),
		}); err != nil {
			return fmt.Errorf("insert user %s: %w", u.Username, err)
		}
	}
	for _, u := range plan.Update {
		quota, err := json.Marshal(u.Quota)
		if err != nil {
			return err
		}
		if err := q.UpdateUser(ctx, gen.UpdateUserParams{
			ID: string(u.ID), PasswordHash: u.PasswordHash, QuotaJson: string(quota),
			Disabled: boolInt(u.Disabled), ConfigVersion: u.ConfigVersion, UpdatedAt: ms(u.UpdatedAt),
		}); err != nil {
			return fmt.Errorf("update user %s: %w", u.Username, err)
		}
	}
	for _, id := range plan.RevokeSessions {
		if err := q.DeleteSessionsForUser(ctx, string(id)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Create(ctx context.Context, sess domain.Session) error {
	return s.w.InsertSession(ctx, gen.InsertSessionParams{
		ID: sess.ID[:], UserID: string(sess.UserID), CsrfSecret: sess.CSRFSecret,
		CreatedAt: ms(sess.CreatedAt), LastSeenAt: ms(sess.LastSeenAt), ExpiresAt: ms(sess.ExpiresAt),
	})
}

func (s *Store) Get(ctx context.Context, id domain.SessionID) (domain.Session, error) {
	r, err := s.r.SessionByID(ctx, id[:])
	if err != nil {
		return domain.Session{}, notFound(err, domain.ErrSessionNotFound)
	}
	var sid domain.SessionID
	if copy(sid[:], r.ID) != len(sid) {
		return domain.Session{}, fmt.Errorf("session id of length %d", len(r.ID))
	}
	return domain.Session{
		ID: sid, UserID: domain.UserID(r.UserID), CSRFSecret: r.CsrfSecret,
		CreatedAt: fromMs(r.CreatedAt), LastSeenAt: fromMs(r.LastSeenAt), ExpiresAt: fromMs(r.ExpiresAt),
	}, nil
}

func (s *Store) Touch(ctx context.Context, id domain.SessionID, at time.Time) error {
	return s.w.TouchSession(ctx, gen.TouchSessionParams{LastSeenAt: ms(at), ID: id[:]})
}

func (s *Store) Delete(ctx context.Context, id domain.SessionID) error {
	return s.w.DeleteSession(ctx, id[:])
}

func (s *Store) DeleteInactive(ctx context.Context, now, idleCutoff time.Time) (int64, error) {
	return s.w.DeleteInactiveSessions(ctx, gen.DeleteInactiveSessionsParams{ExpiresAt: ms(now), LastSeenAt: ms(idleCutoff)})
}
