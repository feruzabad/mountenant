package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jobsqlite "github.com/feruzabad/mountenant/internal/jobs/adapters/sqlite"
	"github.com/feruzabad/mountenant/internal/jobs/domain"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/db"
	"github.com/feruzabad/mountenant/internal/platform/id"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

var cfg = Config{
	Retention:          domain.Retention{Ready: 168 * time.Hour, Failed: 24 * time.Hour, ImportTimeout: 30 * time.Minute},
	NZBHardCap:         1 << 20,
	MaxFilesPerJob:     3,
	PollInterval:       10 * time.Second,
	ReadyCheckInterval: time.Hour,
}

var limits = domain.Limits{MaxActiveJobs: 3, MaxTotalJobs: 5, MaxNZBBytes: 1 << 20}

type world struct {
	svc     *Service
	rec     *Reconciler
	jobs    *jobsqlite.Store
	backend *fakeBackend
	clock   *clock.Fake
	db      *db.DB
}

func newWorld(t *testing.T) *world {
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
	for _, u := range []string{"u1", "u2"} {
		d.W.Exec(`INSERT INTO users (id, username, password_hash, quota_json, config_version, created_at, updated_at) VALUES (?, ?, 'h', '{}', 'v', 0, 0)`, u, u)
	}
	w := &world{jobs: jobsqlite.New(d), backend: newFakeBackend(), clock: clock.NewFake(t0), db: d}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w.svc = &Service{Jobs: w.jobs, Backend: w.backend, Clock: w.clock, IDs: &id.UUIDv7{Clock: w.clock}, Config: cfg, Logger: log}
	w.rec = &Reconciler{Jobs: w.jobs, Backend: w.backend, Clock: w.clock, Config: cfg, Logger: log}
	return w
}

func nzbDoc(tag string) string {
	return `<?xml version="1.0"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb"><file subject="` + tag +
		`"><groups><group>a.b</group></groups><segments><segment bytes="10" number="1">x@y</segment></segments></file></nzb>`
}

func (w *world) submit(t *testing.T, owner, tag string) SubmitResult {
	t.Helper()
	res, err := w.svc.Submit(context.Background(), SubmitInput{Owner: domain.OwnerID(owner), Limits: limits, FileName: tag + ".nzb", Body: strings.NewReader(nzbDoc(tag))})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (w *world) round(t *testing.T, advance time.Duration) {
	t.Helper()
	w.clock.Advance(advance)
	if err := w.rec.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (w *world) load(t *testing.T, id domain.JobID) *domain.Job {
	t.Helper()
	j, err := w.jobs.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestLifecycle(t *testing.T) {
	w := newWorld(t)
	res := w.submit(t, "u1", "debian")
	id := res.Job.ID
	if res.Duplicate || string(w.backend.submitted[string(id)]) != nzbDoc("debian") {
		t.Fatal("NZB not submitted as plain XML")
	}
	j := w.load(t, id)
	if j.BackendRef == nil || j.BackendRef.NzoID != "nzo-"+string(id) {
		t.Fatalf("ref %+v", j.BackendRef)
	}
	if _, err := w.jobs.NZB(context.Background(), id); !errors.Is(err, domain.ErrNoNZB) {
		t.Fatal("NZB kept after acceptance")
	}

	w.backend.set(string(id), domain.BackendImporting)
	w.round(t, cfg.PollInterval)
	if s := w.load(t, id).Status; s != domain.StatusImporting {
		t.Fatalf("status %s", s)
	}

	w.backend.set(string(id), domain.BackendCompleted, domain.BackendFile{RelPath: "debian.iso", Size: 100})
	w.round(t, cfg.PollInterval)
	j = w.load(t, id)
	if j.Status != domain.StatusReady || len(j.Files) != 1 || j.Files[0].ContentType != "application/x-iso9660-image" {
		t.Fatalf("ready: %+v", j)
	}

	// The hourly ready check notices vanished content.
	w.round(t, cfg.ReadyCheckInterval-time.Minute)
	if w.load(t, id).Status != domain.StatusReady {
		t.Fatal("checked too early")
	}
	delete(w.backend.present, string(id))
	w.round(t, time.Minute)
	j = w.load(t, id)
	if j.Status != domain.StatusFailed || j.Failure.Code != domain.FailContentMissing {
		t.Fatalf("content missing: %+v", j)
	}
}

func TestBackendFailureAndTimeout(t *testing.T) {
	w := newWorld(t)
	failed := w.submit(t, "u1", "a").Job.ID
	stuck := w.submit(t, "u1", "b").Job.ID
	w.backend.set(string(failed), domain.BackendFailed)
	w.round(t, cfg.PollInterval)
	j := w.load(t, failed)
	if j.Status != domain.StatusFailed || j.Failure.Message != "articles missing" {
		t.Fatalf("%+v", j.Failure)
	}
	w.round(t, cfg.Retention.ImportTimeout)
	if j := w.load(t, stuck); j.Status != domain.StatusFailed || j.Failure.Code != domain.FailImportTimeout {
		t.Fatalf("timeout: %s %+v", j.Status, j.Failure)
	}
}

func TestResubmitAfterBackendOutage(t *testing.T) {
	w := newWorld(t)
	w.backend.submitErr = domain.ErrBackendUnavailable
	id := w.submit(t, "u1", "a").Job.ID
	j := w.load(t, id)
	if j.BackendRef != nil || j.Attempts != 1 {
		t.Fatalf("after failed submit: %+v", j)
	}
	if _, err := w.jobs.NZB(context.Background(), id); err != nil {
		t.Fatal("NZB must be kept for the retry")
	}
	w.backend.submitErr = nil
	w.round(t, cfg.PollInterval)
	j = w.load(t, id)
	if j.BackendRef == nil || len(w.backend.submitted) != 1 {
		t.Fatalf("not resubmitted: %+v", j)
	}
	if w.backend.calls[1] != "find "+string(id) {
		t.Fatalf("Find must precede a resubmit: %v", w.backend.calls)
	}
}

func TestResubmitFindsExisting(t *testing.T) {
	// Crash after the backend accepted the NZB but before the reference was
	// saved: the reconciler must adopt the backend job, not submit again.
	w := newWorld(t)
	w.backend.submitErr = domain.ErrBackendUnavailable
	id := w.submit(t, "u1", "a").Job.ID
	w.backend.submitted[string(id)] = []byte("x")
	w.backend.submitErr = errors.New("must not be called")
	w.round(t, cfg.PollInterval)
	if j := w.load(t, id); j.BackendRef == nil || j.BackendRef.NzoID != "nzo-"+string(id) {
		t.Fatalf("not adopted: %+v", j)
	}
}

func TestRejectedSubmission(t *testing.T) {
	w := newWorld(t)
	w.backend.submitErr = domain.ErrBackendRejected
	j := w.load(t, w.submit(t, "u1", "a").Job.ID)
	if j.Status != domain.StatusFailed || j.Failure.Code != domain.FailBackendReject {
		t.Fatalf("%+v", j)
	}
}

func TestDuplicatesAndQuota(t *testing.T) {
	w := newWorld(t)
	first := w.submit(t, "u1", "a")
	gz := w.submit(t, "u1", "a")
	if !gz.Duplicate || gz.Job.ID != first.Job.ID || gz.Job.NZBName != "a.nzb" {
		t.Fatalf("duplicate: %+v", gz)
	}
	if w.submit(t, "u2", "a").Duplicate {
		t.Fatal("dedup across users")
	}
	w.submit(t, "u1", "b")
	w.submit(t, "u1", "c")
	_, err := w.svc.Submit(context.Background(), SubmitInput{Owner: "u1", Limits: limits, FileName: "d", Body: strings.NewReader(nzbDoc("d"))})
	if !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("4th active job: %v", err)
	}
	small := limits
	small.MaxNZBBytes = 10
	_, err = w.svc.Submit(context.Background(), SubmitInput{Owner: "u2", Limits: small, FileName: "e", Body: strings.NewReader(nzbDoc("e"))})
	if !errors.Is(err, domain.ErrNZBTooLarge) {
		t.Fatalf("size limit: %v", err)
	}
	_, err = w.svc.Submit(context.Background(), SubmitInput{Owner: "u2", Limits: limits, FileName: "f", Body: bytes.NewReader([]byte("<html/>"))})
	if !errors.Is(err, domain.ErrInvalidNZB) {
		t.Fatalf("invalid: %v", err)
	}
}

func TestDeleteVerifiesBackendCleanup(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	id := w.submit(t, "u1", "a").Job.ID
	if err := w.svc.Delete(ctx, "u2", id); !errors.Is(err, ErrNotFound) {
		t.Fatal("other user deleted the job")
	}
	if err := w.svc.Delete(ctx, "u1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Get(ctx, "u1", id); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted job still visible")
	}

	// The backend keeps the job at first: cleanup retries with backoff.
	w.backend.sticky = true
	w.round(t, 0)
	j := w.load(t, id)
	if !j.BackendRemovedAt.IsZero() || j.Attempts != 1 {
		t.Fatalf("cleanup counted as done: %+v", j)
	}
	w.backend.sticky = false
	w.round(t, cfg.PollInterval)
	j = w.load(t, id)
	if j.BackendRemovedAt.IsZero() || !j.NextCheckAt.IsZero() {
		t.Fatalf("cleanup not verified: %+v", j)
	}
	if err := w.svc.Delete(ctx, "u1", id); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted twice")
	}
}

func TestExpiry(t *testing.T) {
	w := newWorld(t)
	id := w.submit(t, "u1", "a").Job.ID
	w.backend.set(string(id), domain.BackendCompleted, domain.BackendFile{RelPath: "f", Size: 1})
	w.round(t, cfg.PollInterval)
	// Keep ready checks happy until expiry.
	for i := 0; i < 168; i++ {
		w.round(t, time.Hour)
	}
	j := w.load(t, id)
	if j.Status != domain.StatusDeleted || j.BackendRemovedAt.IsZero() {
		t.Fatalf("not expired and cleaned: %s %+v", j.Status, j)
	}
}

func TestTooManyFiles(t *testing.T) {
	w := newWorld(t)
	id := w.submit(t, "u1", "a").Job.ID
	var files []domain.BackendFile
	for i := 0; i < cfg.MaxFilesPerJob+1; i++ {
		files = append(files, domain.BackendFile{RelPath: string(rune('a' + i)), Size: 1})
	}
	w.backend.set(string(id), domain.BackendCompleted, files...)
	w.round(t, cfg.PollInterval)
	if j := w.load(t, id); j.Failure == nil || j.Failure.Code != domain.FailTooManyFiles {
		t.Fatalf("%+v", j)
	}
}
