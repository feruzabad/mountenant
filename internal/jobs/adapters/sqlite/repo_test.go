package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
	"github.com/feruzabad/mountenant/internal/platform/db"
)

var (
	t0  = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ret = domain.Retention{Ready: 168 * time.Hour, Failed: 24 * time.Hour, ImportTimeout: 30 * time.Minute}
)

func newStore(t *testing.T) (*Store, *db.DB) {
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
		if _, err := d.W.ExecContext(ctx, `INSERT INTO users (id, username, password_hash, quota_json, config_version, created_at, updated_at)
			VALUES (?, ?, 'h', '{}', 'v', 0, 0)`, u, u+"name"); err != nil {
			t.Fatal(err)
		}
	}
	return New(d), d
}

func job(id string, owner string, digest byte, at time.Time) *domain.Job {
	return domain.NewJob(domain.JobID(id), domain.OwnerID(owner), id+".nzb", [32]byte{digest}, at, ret)
}

var nzb = []byte("<nzb/>")

func mustCreate(t *testing.T, s *Store, j *domain.Job) {
	t.Helper()
	if err := s.Create(context.Background(), j, nzb); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, d *db.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.R.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateLoadSave(t *testing.T) {
	s, d := newStore(t)
	ctx := context.Background()
	j := job("j1", "u1", 1, t0)
	if err := s.Create(ctx, j, []byte("<nzb/>")); err != nil {
		t.Fatal(err)
	}
	if nzb, err := s.NZB(ctx, "j1"); err != nil || string(nzb) != "<nzb/>" {
		t.Fatalf("nzb %q %v", nzb, err)
	}
	if n := count(t, d, `SELECT count(*) FROM job_events WHERE job_id = 'j1' AND type = 'job.submitted'`); n != 1 {
		t.Fatalf("submitted events %d", n)
	}

	got, err := s.Get(ctx, "u1", "j1")
	if err != nil {
		t.Fatal(err)
	}
	if got.NZBName != "j1.nzb" || got.Status != domain.StatusQueued || got.NZBDigest != j.NZBDigest || !got.CreatedAt.Equal(t0) || got.Version != 1 {
		t.Fatalf("loaded %+v", got)
	}
	if _, err := s.Get(ctx, "u2", "j1"); !errors.Is(err, domain.ErrJobNotFound) {
		t.Fatal("other owner can load the job")
	}

	// Backend accepted: the NZB blob is dropped (UC-17).
	got.Accepted(domain.BackendRef{JobID: "j1", NzoID: "nzo"}, t0)
	got.StartImport(t0)
	if err := s.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NZB(ctx, "j1"); !errors.Is(err, domain.ErrNoNZB) {
		t.Fatal("blob kept after acceptance")
	}

	files := []domain.JobFile{{RelPath: "a/b c.iso", Size: 5, ContentType: "application/x-iso9660-image"}, {RelPath: "r.txt", Size: 1, ContentType: "text/plain"}}
	got.MarkReady(files, 10, t0.Add(time.Minute), ret)
	if err := s.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	ready, _ := s.Load(ctx, "j1")
	if ready.BackendRef == nil || ready.BackendRef.NzoID != "nzo" || len(ready.Files) != 2 || ready.Files[0].RelPath != "a/b c.iso" || !ready.ReadyAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("ready %+v", ready)
	}

	// Ready → Failed clears the catalogue.
	ready.Fail(domain.Failure{Code: domain.FailContentMissing, Message: "gone"}, t0.Add(time.Hour), ret)
	if err := s.Save(ctx, ready); err != nil {
		t.Fatal(err)
	}
	failed, _ := s.Load(ctx, "j1")
	if failed.Failure == nil || failed.Failure.Message != "gone" || len(failed.Files) != 0 || count(t, d, `SELECT count(*) FROM job_files`) != 0 {
		t.Fatalf("failed %+v", failed)
	}

	var payload Snapshot
	var raw string
	d.R.QueryRow(`SELECT payload_json FROM job_events WHERE type = 'job.failed'`).Scan(&raw)
	json.Unmarshal([]byte(raw), &payload)
	if payload.Status != "failed" || payload.Failure.Code != domain.FailContentMissing {
		t.Fatalf("event payload %s", raw)
	}
}

func TestOptimisticConcurrency(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	mustCreate(t, s, job("j1", "u1", 1, t0))
	a, _ := s.Load(ctx, "j1")
	b, _ := s.Load(ctx, "j1")
	a.StartImport(t0)
	if err := s.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	b.Delete(t0)
	if err := s.Save(ctx, b); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if err := s.Save(ctx, a); err != nil { // version followed the first save
		t.Fatalf("second save of the same aggregate: %v", err)
	}
}

func TestDuplicates(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, job("j1", "u1", 7, t0), nzb); err != nil {
		t.Fatal(err)
	}
	var dup *domain.DuplicateError
	if err := s.Create(ctx, job("j2", "u1", 7, t0), nzb); !errors.As(err, &dup) || dup.Existing.ID != "j1" {
		t.Fatalf("duplicate: %v", err)
	}
	if err := s.Create(ctx, job("j3", "u2", 7, t0), nzb); err != nil {
		t.Fatalf("other owner, same NZB: %v", err)
	}
	j1, _ := s.Load(ctx, "j1")
	j1.Fail(domain.Failure{Code: domain.FailBackendFailed}, t0, ret)
	s.Save(ctx, j1)
	if err := s.Create(ctx, job("j4", "u1", 7, t0), nzb); err != nil {
		t.Fatalf("re-upload after failure: %v", err)
	}

	// Concurrent uploads of one NZB create exactly one job.
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Create(ctx, job(fmt.Sprintf("c%d", i), "u2", 9, t0), nzb) == nil {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("%d jobs created concurrently", created)
	}
}

func TestListCountsDueExpired(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		mustCreate(t, s, job(fmt.Sprintf("j%d", i), "u1", byte(i), t0.Add(time.Duration(i)*time.Minute)))
	}
	mustCreate(t, s, job("other", "u2", 1, t0))
	del, _ := s.Load(ctx, "j0")
	del.Delete(t0)
	s.Save(ctx, del)
	imp, _ := s.Load(ctx, "j1")
	imp.MarkReady([]domain.JobFile{{RelPath: "f"}}, 10, t0, ret)
	s.Save(ctx, imp)

	var ids []string
	cursor := ""
	for {
		page, next, err := s.List(ctx, "u1", domain.ListQuery{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range page {
			ids = append(ids, string(j.ID))
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if fmt.Sprint(ids) != "[j4 j3 j2 j1]" {
		t.Fatalf("pages %v", ids)
	}
	if page, _, _ := s.List(ctx, "u1", domain.ListQuery{Limit: 10, Status: domain.StatusReady}); len(page) != 1 || page[0].ID != "j1" {
		t.Fatalf("status filter %v", page)
	}
	if _, _, err := s.List(ctx, "u1", domain.ListQuery{Limit: 10, Cursor: "!!"}); !errors.Is(err, ErrBadCursor) {
		t.Fatal("bad cursor accepted")
	}

	c, _ := s.Counts(ctx, "u1")
	if c.Active != 3 || c.Total != 4 {
		t.Fatalf("counts %+v", c)
	}

	due, _ := s.Due(ctx, t0.Add(2*time.Minute), 10)
	if len(due) != 4 { // j0 (cleanup), j1 (ready check at t0), j2, other; j3/j4 not yet
		t.Fatalf("due %d", len(due))
	}
	// Queued jobs expire importTimeout+failedRetention after creation; j2 was
	// created at +2m. j1 is Ready, j0 Deleted, j3 and j4 are younger.
	exp, _ := s.Expired(ctx, t0.Add(ret.ImportTimeout+ret.Failed+2*time.Minute), 10)
	if len(exp) != 2 || exp[0].ID != "other" || exp[1].ID != "j2" {
		t.Fatalf("expired %d", len(exp))
	}
	known, _ := s.KnownIDs(ctx)
	if len(known) != 5 || known["j0"] {
		t.Fatalf("known %v", known)
	}
}
