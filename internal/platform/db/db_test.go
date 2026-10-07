package db

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

func open(t *testing.T) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "sub", "test.db"), 5000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenSetsPragmas(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	check := func(name string, q func(string) string, pragma, want string) {
		if got := q(pragma); got != want {
			t.Errorf("%s %s = %q, want %q", name, pragma, got, want)
		}
	}
	w := func(p string) (s string) { d.W.QueryRowContext(ctx, "PRAGMA "+p).Scan(&s); return }
	r := func(p string) (s string) { d.R.QueryRowContext(ctx, "PRAGMA "+p).Scan(&s); return }
	check("writer", w, "journal_mode", "wal")
	check("writer", w, "foreign_keys", "1")
	check("writer", w, "synchronous", "1") // NORMAL
	check("writer", w, "busy_timeout", "5000")
	check("reader", r, "foreign_keys", "1")
	check("reader", r, "query_only", "1")

	if _, err := d.R.ExecContext(ctx, "CREATE TABLE x (a)"); err == nil {
		t.Error("reader pool can write")
	}
	if err := d.CheckWritable(ctx); err != nil {
		t.Error(err)
	}
}

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, d, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	v, err := Version(ctx, d)
	if err != nil || v < 1 {
		t.Fatalf("version %d, %v", v, err)
	}
	for _, table := range []string{"users", "sessions", "jobs", "job_files", "job_events", "usage_daily", "nzb_blobs"} {
		var n int
		if err := d.R.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Errorf("%s: %v", table, err)
		}
	}
}

func TestSchemaConstraints(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	if err := Migrate(ctx, d, nil); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) error { _, err := d.W.ExecContext(ctx, q, args...); return err }
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO users (id, username, password_hash, quota_json, config_version, created_at, updated_at)
	          VALUES ('u1', 'Alice', 'h', '{}', 'v', 1, 1)`)
	if exec(`INSERT INTO users (id, username, password_hash, quota_json, config_version, created_at, updated_at)
	         VALUES ('u2', 'alice', 'h', '{}', 'v', 1, 1)`) == nil {
		t.Error("username uniqueness is not case-insensitive")
	}

	job := `INSERT INTO jobs (id, owner_id, nzb_name, nzb_digest, status, failure_code, created_at, updated_at, expires_at)
	        VALUES (?, ?, 'n.nzb', x'01', ?, ?, 1, 1, ?)`
	mustExec(job, "j1", "u1", "queued", nil, 10)
	if exec(job, "j2", "u1", "ready", nil, 10) == nil {
		t.Error("second live job with the same digest accepted")
	}
	mustExec(job, "j3", "u1", "failed", "backend_failed", 10) // failed jobs are not duplicates
	if exec(job, "j4", "u1", "Queued", nil, 10) == nil {
		t.Error("status CHECK not enforced")
	}
	if exec(job, "j5", "u1", "failed", nil, 10) == nil {
		t.Error("failed job without failure_code accepted")
	}
	if exec(job, "j6", "u1", "importing", nil, nil) == nil {
		t.Error("non-deleted job without expires_at accepted")
	}
	if exec(job, "j7", "nobody", "queued", nil, 10) == nil {
		t.Error("foreign key on owner_id not enforced")
	}

	mustExec(`INSERT INTO job_files (job_id, rel_path, size, content_type) VALUES ('j1', 'a/b.iso', 5, 'x')`)
	mustExec(`INSERT INTO nzb_blobs (job_id, content) VALUES ('j1', x'00')`)
	mustExec(`DELETE FROM jobs WHERE id = 'j1'`)
	var n int
	d.W.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM job_files) + (SELECT count(*) FROM nzb_blobs)`).Scan(&n)
	if n != 0 {
		t.Error("ON DELETE CASCADE not applied")
	}
}

func TestWriterSerialisesConcurrentWrites(t *testing.T) {
	d := open(t)
	ctx := context.Background()
	if _, err := d.W.ExecContext(ctx, "CREATE TABLE c (n INTEGER)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := d.W.BeginTx(ctx, nil)
			if err != nil {
				errs <- err
				return
			}
			var n int
			tx.QueryRowContext(ctx, "SELECT count(*) FROM c").Scan(&n)
			if _, err := tx.ExecContext(ctx, "INSERT INTO c VALUES (?)", n); err != nil {
				tx.Rollback()
				errs <- err
				return
			}
			errs <- tx.Commit()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var distinct int
	d.R.QueryRowContext(ctx, "SELECT count(DISTINCT n) FROM c").Scan(&distinct)
	if distinct != 50 {
		t.Fatalf("lost updates: %d distinct values", distinct)
	}
}

func TestFailedMigrationAborts(t *testing.T) {
	d := open(t)
	bad := fstest.MapFS{
		"00001_ok.sql":  {Data: []byte("-- +goose Up\nCREATE TABLE a (x);\n")},
		"00002_bad.sql": {Data: []byte("-- +goose Up\nCREATE TABLE b (x);\nTHIS IS NOT SQL;\n")},
	}
	err := migrate(context.Background(), d.W, bad, nil)
	if err == nil || !strings.Contains(err.Error(), "version:2") {
		t.Fatalf("got %v", err)
	}
	var n int
	d.R.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE name = 'b'").Scan(&n)
	if n != 0 {
		t.Error("failed migration left table b behind; it must run in a transaction")
	}
}
