package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/contenttype"
)

const (
	// maxBackoff caps retry delays for submits, polls and cleanups.
	maxBackoff = 10 * time.Minute
	// batchSize bounds the jobs handled per round.
	batchSize = 50
	// cleanupAlertAfter is when failing backend cleanups are logged as
	// errors for alerting (spec §10.4).
	cleanupAlertAfter = 5
)

// Reconciler implements UC-14 (reconcile status), UC-15 (expire jobs) and
// the backend side of UC-13 (verified deletion, ADR 0004). It is
// idempotent and keeps all state in the database, so restarts are safe.
type Reconciler struct {
	Jobs    domain.JobRepository
	Backend domain.Backend
	Clock   clock.Clock
	Config  Config
	Logger  *slog.Logger
}

// Run reconciles every PollInterval, and at once when nudge receives.
func (r *Reconciler) Run(ctx context.Context, nudge <-chan struct{}) {
	t := time.NewTicker(r.Config.PollInterval)
	defer t.Stop()
	for {
		if err := r.Round(ctx); err != nil && ctx.Err() == nil {
			r.Logger.Error("reconcile round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-nudge:
		}
	}
}

// Round expires jobs, then handles every due job once.
func (r *Reconciler) Round(ctx context.Context) error {
	now := r.Clock.Now()
	expired, err := r.Jobs.Expired(ctx, now, batchSize)
	if err != nil {
		return err
	}
	for _, j := range expired {
		if err := j.Delete(now); err != nil {
			continue
		}
		r.save(ctx, j, "expired")
	}
	due, err := r.Jobs.Due(ctx, now, batchSize)
	if err != nil {
		return err
	}
	for _, j := range due {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.handle(ctx, j)
	}
	return nil
}

func (r *Reconciler) save(ctx context.Context, j *domain.Job, why string) {
	switch err := r.Jobs.Save(ctx, j); {
	case errors.Is(err, domain.ErrConflict):
		// A user changed the job meanwhile; the next round sees the new state.
	case err != nil:
		r.Logger.Error("saving job", "job", j.ID, "after", why, "err", err)
	}
}

func (r *Reconciler) handle(ctx context.Context, j *domain.Job) {
	now := r.Clock.Now()
	switch {
	case j.Status == domain.StatusDeleted:
		r.cleanup(ctx, j, now)
	case j.ImportTimedOut(now, r.Config.Retention):
		_ = j.Fail(domain.Failure{Code: domain.FailImportTimeout, Message: "The backend did not finish the import in time."}, now, r.Config.Retention)
		r.save(ctx, j, "timeout")
	case j.Status.InProgress() && j.BackendRef == nil:
		r.resubmit(ctx, j, now)
	case j.Status.InProgress():
		r.poll(ctx, j, now)
	case j.Status == domain.StatusReady:
		r.checkReady(ctx, j, now)
	default:
		j.ScheduleCheck(time.Time{}) // Failed: nothing to do
		r.save(ctx, j, "unschedule")
	}
}

// resubmit retries a submission that did not reach the backend. Find runs
// first: re-submitting a job the backend already has would duplicate it
// (spec §10.4).
func (r *Reconciler) resubmit(ctx context.Context, j *domain.Job, now time.Time) {
	ref, found, err := r.Backend.Find(ctx, string(j.ID))
	if err != nil {
		r.retryLater(ctx, j, now, "find", err)
		return
	}
	if found {
		_ = j.Accepted(ref, now)
		j.AttemptSucceeded()
		j.ScheduleCheck(now)
		r.save(ctx, j, "found")
		return
	}
	nzb, err := r.Jobs.NZB(ctx, j.ID)
	if errors.Is(err, domain.ErrNoNZB) {
		_ = j.Fail(domain.Failure{Code: domain.FailBackendFailed, Message: "The NZB could not be handed to the backend."}, now, r.Config.Retention)
		r.save(ctx, j, "no nzb")
		return
	}
	if err != nil {
		r.Logger.Error("loading NZB", "job", j.ID, "err", err)
		return
	}
	svc := Service{Jobs: r.Jobs, Backend: r.Backend, Clock: r.Clock, Config: r.Config, Logger: r.Logger}
	svc.submitToBackend(ctx, j, nzb)
}

// poll maps the backend status (spec §10.3).
func (r *Reconciler) poll(ctx context.Context, j *domain.Job, now time.Time) {
	st, err := r.Backend.Status(ctx, *j.BackendRef)
	if err != nil {
		r.retryLater(ctx, j, now, "status", err)
		return
	}
	j.AttemptSucceeded()
	next := now.Add(r.Config.PollInterval)
	switch st.State {
	case domain.BackendQueued:
	case domain.BackendImporting:
		if j.Status == domain.StatusQueued {
			_ = j.StartImport(now)
		}
	case domain.BackendCompleted:
		r.complete(ctx, j, now)
		return
	case domain.BackendFailed:
		msg := st.FailMessage
		if msg == "" {
			msg = "The backend could not import the NZB."
		}
		_ = j.Fail(domain.Failure{Code: domain.FailBackendFailed, Message: msg}, now, r.Config.Retention)
		r.save(ctx, j, "backend failed")
		return
	case domain.BackendNotFound:
		_ = j.Fail(domain.Failure{Code: domain.FailBackendFailed, Message: "The backend no longer knows this job."}, now, r.Config.Retention)
		r.save(ctx, j, "not found")
		return
	default:
		// Unknown states leave the job unchanged (spec §10.3).
		r.Logger.Warn("unknown backend status", "job", j.ID, "raw", st.Raw)
	}
	j.ScheduleCheck(next)
	r.save(ctx, j, "poll")
}

// complete catalogues the files of a completed import. Completed with no
// files yet keeps polling until the import timeout.
func (r *Reconciler) complete(ctx context.Context, j *domain.Job, now time.Time) {
	files, err := r.Backend.ListFiles(ctx, *j.BackendRef)
	if errors.Is(err, domain.ErrContentMissing) {
		files, err = nil, nil
	}
	if err != nil {
		r.retryLater(ctx, j, now, "list files", err)
		return
	}
	catalogue := make([]domain.JobFile, len(files))
	for i, f := range files {
		catalogue[i] = domain.JobFile{RelPath: f.RelPath, Size: f.Size, ContentType: contenttype.ForPath(f.RelPath)}
	}
	switch err := j.MarkReady(catalogue, r.Config.MaxFilesPerJob, now, r.Config.Retention); {
	case err == nil:
		j.ScheduleCheck(now.Add(r.Config.ReadyCheckInterval))
		r.save(ctx, j, "ready")
	case errors.Is(err, domain.ErrNoFiles):
		j.ScheduleCheck(now.Add(r.Config.PollInterval))
		r.save(ctx, j, "no files yet")
	case errors.Is(err, domain.ErrTooManyFiles):
		_ = j.Fail(domain.Failure{Code: domain.FailTooManyFiles, Message: "The job has more files than this server allows."}, now, r.Config.Retention)
		r.save(ctx, j, "too many files")
	default:
		r.Logger.Error("cataloguing files", "job", j.ID, "err", err)
		_ = j.Fail(domain.Failure{Code: domain.FailBackendFailed, Message: "The backend listed invalid file names."}, now, r.Config.Retention)
		r.save(ctx, j, "bad files")
	}
}

// checkReady re-verifies a Ready job's content; backend history is not
// trusted for this (spec UC-14).
func (r *Reconciler) checkReady(ctx context.Context, j *domain.Job, now time.Time) {
	files, err := r.Backend.ListFiles(ctx, *j.BackendRef)
	missing := errors.Is(err, domain.ErrContentMissing)
	if err != nil && !missing {
		r.retryLater(ctx, j, now, "ready check", err)
		return
	}
	if !missing {
		listed := make(map[string]int64, len(files))
		for _, f := range files {
			listed[f.RelPath] = f.Size
		}
		for _, f := range j.Files {
			if size, ok := listed[f.RelPath]; !ok || size != f.Size {
				missing = true
				break
			}
		}
	}
	if missing {
		r.Logger.Warn("content of a ready job is gone", "job", j.ID)
		_ = j.Fail(domain.Failure{Code: domain.FailContentMissing, Message: "The files are no longer available on the backend."}, now, r.Config.Retention)
		r.save(ctx, j, "content missing")
		return
	}
	j.AttemptSucceeded()
	j.ScheduleCheck(now.Add(r.Config.ReadyCheckInterval))
	r.save(ctx, j, "ready check")
}

// cleanup removes a deleted job from the backend. Success is VerifyGone,
// never a response code (ADR 0004).
func (r *Reconciler) cleanup(ctx context.Context, j *domain.Job, now time.Time) {
	ref := domain.BackendRef{JobID: string(j.ID)}
	if j.BackendRef != nil {
		ref = *j.BackendRef
	} else if found, ok, err := r.Backend.Find(ctx, string(j.ID)); err != nil {
		r.retryLater(ctx, j, now, "find for cleanup", err)
		return
	} else if ok {
		ref = found // submitted, but the reference was never saved
	}
	gone, err := r.Backend.VerifyGone(ctx, ref)
	if err == nil && !gone {
		if derr := r.Backend.Delete(ctx, ref); derr != nil {
			r.Logger.Info("backend delete reported an error; verifying", "job", j.ID, "err", derr)
		}
		gone, err = r.Backend.VerifyGone(ctx, ref)
	}
	if err != nil || !gone {
		if err == nil {
			err = errors.New("still present after delete")
		}
		r.retryLater(ctx, j, now, "cleanup", err)
		return
	}
	_ = j.BackendRemoved(now)
	r.save(ctx, j, "cleanup")
}

func (r *Reconciler) retryLater(ctx context.Context, j *domain.Job, now time.Time, what string, err error) {
	n := j.AttemptFailed(now, r.Config.PollInterval, maxBackoff)
	level := slog.LevelWarn
	if j.Status == domain.StatusDeleted && n >= cleanupAlertAfter {
		level = slog.LevelError // repeated cleanup failures need an operator
	}
	r.Logger.Log(ctx, level, "backend call failed; retrying", "job", j.ID, "step", what, "attempt", n, "next", j.NextCheckAt, "err", err)
	r.save(ctx, j, what)
}
