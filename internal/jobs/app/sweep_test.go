package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

type listingBackend struct {
	*fakeBackend
	listing domain.NamespaceListing
	deleted []domain.BackendRef
}

func (l *listingBackend) ListNamespace(context.Context) (domain.NamespaceListing, error) {
	return l.listing, nil
}

func (l *listingBackend) Delete(_ context.Context, ref domain.BackendRef) error {
	l.deleted = append(l.deleted, ref)
	return nil
}

func TestSweep(t *testing.T) {
	w := newWorld(t)
	known := w.submit(t, "u1", "a").Job.ID
	now := t0.Add(48 * time.Hour)
	old := now.Add(-time.Hour)
	const orphanID = "0192f0e0-0000-7000-8000-00000000000a"
	const youngID = "0192f0e0-0000-7000-8000-00000000000b"
	lb := &listingBackend{fakeBackend: w.backend, listing: domain.NamespaceListing{
		BackendNow: now,
		Dirs: []domain.NamespaceEntry{
			{Name: string(known), ModTime: old},
			{Name: orphanID, ModTime: old},
			{Name: orphanID + " (2)", ModTime: old},
			{Name: youngID, ModTime: now.Add(-time.Minute)},
			{Name: "Some.Release.Someone.Put.Here", ModTime: old},
		},
		History: []domain.NamespaceEntry{
			{Name: orphanID, NzoID: "nzo-orphan", ModTime: old},
			{Name: string(known), NzoID: "nzo-known", ModTime: old},
		},
	}}
	var logs strings.Builder
	s := &Sweeper{Jobs: w.jobs, Backend: lb, ImportTimeout: 30 * time.Minute, Logger: slog.New(slog.NewTextHandler(&logs, nil))}

	s.DryRun = true
	res, err := s.Sweep(context.Background())
	if err != nil || len(lb.deleted) != 0 || strings.Count(logs.String(), "would delete") != 2 {
		t.Fatalf("dry run: %+v %v %d deletes\n%s", res, err, len(lb.deleted), logs.String())
	}

	s.DryRun = false
	res, _ = s.Sweep(context.Background())
	if res.Deleted != 2 || res.Skipped != 1 || res.Considered != 6 {
		t.Fatalf("result %+v", res)
	}
	got := map[string]string{}
	for _, r := range lb.deleted {
		got[r.JobID] = r.NzoID
	}
	if got[orphanID] != "nzo-orphan" || len(got) != 2 {
		t.Fatalf("deleted %v", got)
	}
	if _, ok := got[orphanID+" (2)"]; !ok {
		t.Fatalf("duplicate import not removed: %v", got)
	}
}

func TestSweepWithoutBackendClock(t *testing.T) {
	w := newWorld(t)
	lb := &listingBackend{fakeBackend: w.backend, listing: domain.NamespaceListing{
		Dirs: []domain.NamespaceEntry{{Name: "0192f0e0-0000-7000-8000-00000000000a", ModTime: t0}},
	}}
	s := &Sweeper{Jobs: w.jobs, Backend: lb, ImportTimeout: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := s.Sweep(context.Background()); err != nil || len(lb.deleted) != 0 {
		t.Fatal("swept without a backend clock")
	}
}

func TestPurgeNZBs(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.backend.submitErr = domain.ErrBackendUnavailable
	held := w.submit(t, "u1", "a").Job.ID // retry pending: keep
	j := w.load(t, w.submit(t, "u1", "b").Job.ID)
	// Simulate a crash that left a blob next to a failed job.
	w.db.W.Exec(`UPDATE jobs SET status = 'failed', failure_code = 'x', failed_at = 1 WHERE id = ?`, j.ID)
	n, err := w.jobs.PurgeNZBs(ctx)
	if err != nil || n != 1 {
		t.Fatalf("purged %d %v", n, err)
	}
	if _, err := w.jobs.NZB(ctx, held); err != nil {
		t.Fatal("pending NZB purged")
	}
}
