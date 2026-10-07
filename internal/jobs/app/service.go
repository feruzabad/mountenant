// Package app holds the Jobs use cases (spec §5.2).
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/id"
)

// Config holds the job settings from config.
type Config struct {
	Retention          domain.Retention
	NZBHardCap         int64         // jobs.maxNzbBytesHardCap
	MaxFilesPerJob     int           // jobs.maxFilesPerJob
	PollInterval       time.Duration // backend.pollInterval
	ReadyCheckInterval time.Duration // jobs.readyCheckInterval
}

// Service implements UC-10 to UC-13.
type Service struct {
	Jobs    domain.JobRepository
	Backend domain.Backend
	Clock   clock.Clock
	IDs     id.Generator
	Config  Config
	Logger  *slog.Logger
	// Nudge, if set, is called after a change the reconciler should handle
	// soon (a new job, a deletion). It must not block.
	Nudge func()
}

// ErrNotFound hides jobs that do not exist, belong to someone else or are
// deleted (spec §6.2: ownership failures are 404).
var ErrNotFound = errors.New("job not found")

// SubmitInput is one upload.
type SubmitInput struct {
	Owner    domain.OwnerID
	Limits   domain.Limits
	FileName string
	Body     io.Reader
}

// SubmitResult is the created or existing job.
type SubmitResult struct {
	Job       *domain.Job
	Duplicate bool
}

// Submit implements UC-10. The job is committed as Queued before the NZB is
// handed to the backend, so a crash in between leaves a job the reconciler
// resubmits (after Find) and the orphan sweep never removes a submitted job
// that has no row.
func (s *Service) Submit(ctx context.Context, in SubmitInput) (SubmitResult, error) {
	nzb, err := domain.ReadNZB(in.Body, domain.EffectiveNZBLimit(in.Limits.MaxNZBBytes, s.Config.NZBHardCap))
	if err != nil {
		return SubmitResult{}, err
	}
	counts, err := s.Jobs.Counts(ctx, in.Owner)
	if err != nil {
		return SubmitResult{}, err
	}
	if err := domain.CheckSubmission(in.Limits, counts); err != nil {
		return SubmitResult{}, err
	}
	now := s.Clock.Now()
	job := domain.NewJob(domain.JobID(s.IDs.New()), in.Owner, in.FileName, nzb.Digest, now, s.Config.Retention)
	var dup *domain.DuplicateError
	switch err := s.Jobs.Create(ctx, job, nzb.XML); {
	case errors.As(err, &dup):
		return SubmitResult{Job: dup.Existing, Duplicate: true}, nil
	case err != nil:
		return SubmitResult{}, err
	}
	// Hand the NZB over right away for fast feedback; if the backend is down
	// the reconciler retries.
	s.submitToBackend(ctx, job, nzb.XML)
	if s.Nudge != nil {
		s.Nudge()
	}
	return SubmitResult{Job: job}, nil
}

// submitToBackend submits a Queued job without a backend reference and
// saves the outcome. Errors are recorded on the job, not returned: the job
// exists either way.
func (s *Service) submitToBackend(ctx context.Context, job *domain.Job, nzb []byte) {
	now := s.Clock.Now()
	ref, err := s.Backend.Submit(ctx, nzb, string(job.ID))
	switch {
	case err == nil:
		_ = job.Accepted(ref, now)
		job.AttemptSucceeded()
		job.ScheduleCheck(now.Add(s.Config.PollInterval))
	case errors.Is(err, domain.ErrBackendRejected):
		s.Logger.Warn("backend rejected NZB", "job", job.ID, "err", err)
		_ = job.Fail(domain.Failure{Code: domain.FailBackendReject, Message: "The backend rejected the NZB."}, now, s.Config.Retention)
	default:
		s.Logger.Warn("submitting NZB failed; will retry", "job", job.ID, "err", err)
		job.AttemptFailed(now, s.Config.PollInterval, maxBackoff)
	}
	if err := s.Jobs.Save(ctx, job); err != nil && !errors.Is(err, domain.ErrConflict) {
		s.Logger.Error("saving job after submit", "job", job.ID, "err", err)
	}
}

// List implements UC-11.
func (s *Service) List(ctx context.Context, owner domain.OwnerID, q domain.ListQuery) ([]*domain.Job, string, error) {
	return s.Jobs.List(ctx, owner, q)
}

// Get implements UC-12.
func (s *Service) Get(ctx context.Context, owner domain.OwnerID, id domain.JobID) (*domain.Job, error) {
	j, err := s.Jobs.Get(ctx, owner, id)
	if errors.Is(err, domain.ErrJobNotFound) || err == nil && j.Status == domain.StatusDeleted {
		return nil, ErrNotFound
	}
	return j, err
}

// Delete implements UC-13: the job is marked Deleted at once (which also
// revokes its download links, since link verification re-checks the job)
// and the reconciler removes it from the backend and verifies that.
func (s *Service) Delete(ctx context.Context, owner domain.OwnerID, id domain.JobID) error {
	for attempt := 0; ; attempt++ {
		j, err := s.Get(ctx, owner, id)
		if err != nil {
			return err
		}
		if err := j.Delete(s.Clock.Now()); err != nil {
			return err
		}
		err = s.Jobs.Save(ctx, j)
		if errors.Is(err, domain.ErrConflict) && attempt < 3 {
			continue // the reconciler touched it; reload and try again
		}
		if err != nil {
			return fmt.Errorf("delete job %s: %w", id, err)
		}
		if s.Nudge != nil {
			s.Nudge()
		}
		return nil
	}
}
