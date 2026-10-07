// Package db opens the SQLite database (spec §9.1) and applies the embedded
// migrations (spec §9.2).
//
// SQLite under WAL allows one writer and many readers, so DB holds two pools:
// a single writer connection whose transactions start with BEGIN IMMEDIATE,
// and a read-only pool. Using the writer for every write avoids SQLITE_BUSY
// from lock upgrades.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/feruzabad/mountenant/migrations"
)

// DB is the pair of connection pools.
type DB struct {
	// W is the only connection that writes. Its transactions are
	// BEGIN IMMEDIATE.
	W *sql.DB
	// R is a read-only pool.
	R *sql.DB
}

// Open opens (and creates) the database at path with the pragmas of spec
// §9.1. It does not migrate.
func Open(ctx context.Context, path string, busyTimeoutMs int) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("database directory: %w", err)
		}
	}
	w, err := sql.Open("sqlite", dsn(path, busyTimeoutMs, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	w.SetConnMaxIdleTime(0)

	// The writer creates the file and switches it to WAL before any reader
	// connects.
	var mode string
	if err := w.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if mode != "wal" {
		w.Close()
		return nil, fmt.Errorf("open %s: journal_mode is %q, want wal", path, mode)
	}

	r, err := sql.Open("sqlite", dsn(path, busyTimeoutMs, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(max(4, runtime.GOMAXPROCS(0)))
	if err := r.PingContext(ctx); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	return &DB{W: w, R: r}, nil
}

func dsn(path string, busyTimeoutMs int, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMs))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Set("_txlock", "immediate")
	}
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + q.Encode()
}

// Close closes both pools.
func (d *DB) Close() error {
	return errors.Join(d.R.Close(), d.W.Close())
}

// CheckWritable takes and releases the write lock, for /readyz ("DB
// writable", spec §12.3).
func (d *DB) CheckWritable(ctx context.Context) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Rollback()
}

// Migrate applies all pending migrations from the embedded set, each in its
// own transaction. It is called by `mountenant migrate` and at startup by
// `mountenant serve`; startup aborts on failure.
func Migrate(ctx context.Context, d *DB, log *slog.Logger) error {
	return migrate(ctx, d.W, migrations.FS, log)
}

func migrate(ctx context.Context, w *sql.DB, fsys fs.FS, log *slog.Logger) error {
	p, err := goose.NewProvider(goose.DialectSQLite3, w, fsys)
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	res, err := p.Up(ctx)
	for _, r := range res {
		if r.Error == nil && log != nil {
			log.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration)
		}
	}
	if err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	return nil
}

// Version returns the current schema version (0 for an empty database).
func Version(ctx context.Context, d *DB) (int64, error) {
	p, err := goose.NewProvider(goose.DialectSQLite3, d.W, migrations.FS)
	if err != nil {
		return 0, err
	}
	return p.GetDBVersion(ctx)
}
