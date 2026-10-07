package domain

import (
	"context"
	"errors"
	"time"
)

// Repository errors.
var (
	ErrJobNotFound = errors.New("job not found")
	// ErrConflict means the job changed since it was loaded; reload and
	// retry.
	ErrConflict = errors.New("job changed concurrently")
	// ErrNoNZB means the NZB blob is gone (the backend accepted it, or the
	// job is terminal).
	ErrNoNZB = errors.New("NZB no longer held")
)

// DuplicateError is returned by Create when the owner already has a live
// job with the same NZB digest (spec §4.2).
type DuplicateError struct{ Existing *Job }

func (e *DuplicateError) Error() string { return "duplicate NZB of job " + string(e.Existing.ID) }

// ListQuery selects a page of an owner's jobs, newest first.
type ListQuery struct {
	Status Status // empty for all except Deleted
	Limit  int
	Cursor string // opaque, from the previous page
}

// JobRepository persists jobs (spec §4.5). Every write stores the job's
// pending domain events in the outbox in the same transaction.
type JobRepository interface {
	// Create stores a new job and its NZB.
	Create(ctx context.Context, j *Job, nzb []byte) error
	// Get loads a job of owner; jobs of other owners are ErrJobNotFound.
	Get(ctx context.Context, owner OwnerID, id JobID) (*Job, error)
	// Load loads any job, for background workers only.
	Load(ctx context.Context, id JobID) (*Job, error)
	// List returns a page of the owner's jobs and the cursor of the next
	// page ("" when there is none). Deleted jobs are never listed.
	List(ctx context.Context, owner OwnerID, q ListQuery) ([]*Job, string, error)
	// Counts returns the owner's active and total jobs (SubmissionPolicy).
	Counts(ctx context.Context, owner OwnerID) (Counts, error)
	// Save writes a loaded job back; ErrConflict if it changed meanwhile.
	Save(ctx context.Context, j *Job) error
	// Due returns jobs whose NextCheckAt has passed, oldest first.
	Due(ctx context.Context, now time.Time, limit int) ([]*Job, error)
	// Expired returns jobs that are not Deleted and past ExpiresAt.
	Expired(ctx context.Context, now time.Time, limit int) ([]*Job, error)
	// NZB returns the held NZB of a job, or ErrNoNZB.
	NZB(ctx context.Context, id JobID) ([]byte, error)
	// KnownIDs returns the IDs of all jobs that are not Deleted, for the
	// orphan sweep.
	KnownIDs(ctx context.Context) (map[JobID]bool, error)
}
