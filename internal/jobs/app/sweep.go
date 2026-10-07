package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

// SweepResult summarises one orphan sweep.
type SweepResult struct {
	Considered, Deleted, Skipped int
}

// Sweeper implements UC-16: it removes backend items in the namespace that
// no job accounts for (a crash after submit, failed deletes, manual changes).
type Sweeper struct {
	Jobs          domain.JobRepository
	Backend       domain.Backend
	ImportTimeout time.Duration
	DryRun        bool
	Logger        *slog.Logger
}

// Run sweeps after initialDelay and then every interval. It runs in one
// goroutine, so sweeps never overlap.
func (s *Sweeper) Run(ctx context.Context, initialDelay, interval time.Duration) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		res, err := s.Sweep(ctx)
		if err != nil {
			s.Logger.Error("orphan sweep failed", "err", err)
		} else {
			s.Logger.Info("orphan sweep done", "considered", res.Considered, "deleted", res.Deleted, "skipped", res.Skipped, "dryRun", s.DryRun)
		}
		timer.Reset(interval)
	}
}

// Sweep runs once.
func (s *Sweeper) Sweep(ctx context.Context) (SweepResult, error) {
	// Race safety (spec UC-16): list the backend first, then read the jobs.
	// A job created after the listing cannot be in it, and a job created
	// before it is already in the database (jobs are committed before they
	// are submitted).
	listing, err := s.Backend.ListNamespace(ctx)
	if err != nil {
		return SweepResult{}, err
	}
	known, err := s.Jobs.KnownIDs(ctx)
	if err != nil {
		return SweepResult{}, err
	}
	var res SweepResult
	if listing.BackendNow.IsZero() {
		// Without the backend's clock no entry's age can be judged.
		s.Logger.Warn("orphan sweep skipped: backend sent no Date header")
		return res, nil
	}
	type orphan struct {
		ref    domain.BackendRef
		reason string
	}
	orphans := map[string]orphan{}
	consider := func(e domain.NamespaceEntry, kind string) {
		id, ok := domain.ParseNamespaceName(e.Name)
		if !ok {
			return // not ours: never touched
		}
		res.Considered++
		if known[id] {
			return
		}
		if e.ModTime.IsZero() || listing.BackendNow.Sub(e.ModTime) < s.ImportTimeout {
			res.Skipped++ // may belong to a job in flight
			return
		}
		o := orphans[e.Name]
		o.ref.JobID = e.Name
		if e.NzoID != "" {
			o.ref.NzoID = e.NzoID
		}
		o.reason = kind
		orphans[e.Name] = o
	}
	for _, e := range listing.Dirs {
		consider(e, "directory")
	}
	for _, e := range listing.History {
		consider(e, "history")
	}
	for name, o := range orphans {
		if s.DryRun {
			s.Logger.Info("orphan sweep would delete", "name", name, "nzo", o.ref.NzoID, "found", o.reason)
			continue
		}
		if err := s.Backend.Delete(ctx, o.ref); err != nil {
			s.Logger.Warn("deleting orphan", "name", name, "err", err)
			continue
		}
		s.Logger.Info("orphan deleted", "name", name, "nzo", o.ref.NzoID, "found", o.reason)
		res.Deleted++
	}
	return res, nil
}
