// Package sqlite implements the JobRepository on SQLite. Queries are in
// queries.sql and compiled by sqlc into package gen.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/adapters/sqlite/gen"
	"github.com/feruzabad/mountenant/internal/jobs/domain"
	"github.com/feruzabad/mountenant/internal/platform/db"
)

// Store implements domain.JobRepository.
type Store struct {
	db *db.DB
	r  *gen.Queries
	w  *gen.Queries
}

var _ domain.JobRepository = (*Store)(nil)

// New returns a Store on d.
func New(d *db.DB) *Store {
	return &Store{db: d, r: gen.New(d.R), w: gen.New(d.W)}
}

// Snapshot is the payload stored with each outbox event: the job as it was
// when the event was raised, without its file list.
type Snapshot struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Failure   *Failure  `json:"failure,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	ReadyAt   time.Time `json:"readyAt,omitzero"`
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
}

// Failure is the snapshot form of domain.Failure.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func snapshot(j *domain.Job) Snapshot {
	s := Snapshot{ID: string(j.ID), Name: j.NZBName, Status: string(j.Status), CreatedAt: j.CreatedAt, ReadyAt: j.ReadyAt, ExpiresAt: j.ExpiresAt}
	if j.Failure != nil {
		s.Failure = &Failure{Code: j.Failure.Code, Message: j.Failure.Message}
	}
	return s
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func optMs(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }

func fromOptMs(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMs(v.Int64)
}

func optStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// row holds the columns Save and Create write.
type row struct {
	failureCode, failureMessage, backendRef sql.NullString
}

func encodeRow(j *domain.Job) (row, error) {
	var r row
	if j.Failure != nil {
		r.failureCode = sql.NullString{String: j.Failure.Code, Valid: true}
		r.failureMessage = sql.NullString{String: j.Failure.Message, Valid: true}
	}
	if j.BackendRef != nil {
		b, err := json.Marshal(j.BackendRef)
		if err != nil {
			return r, err
		}
		r.backendRef = optStr(string(b))
	}
	return r, nil
}

func toJob(g gen.Job, files []gen.JobFile) (*domain.Job, error) {
	j := &domain.Job{
		ID:               domain.JobID(g.ID),
		Owner:            domain.OwnerID(g.OwnerID),
		NZBName:          g.NzbName,
		Status:           domain.Status(g.Status),
		CreatedAt:        fromMs(g.CreatedAt),
		UpdatedAt:        fromMs(g.UpdatedAt),
		ReadyAt:          fromOptMs(g.ReadyAt),
		FailedAt:         fromOptMs(g.FailedAt),
		ExpiresAt:        fromOptMs(g.ExpiresAt),
		NextCheckAt:      fromOptMs(g.NextCheckAt),
		Attempts:         int(g.Attempts),
		BackendRemovedAt: fromOptMs(g.BackendRemovedAt),
		Version:          g.Version,
	}
	if copy(j.NZBDigest[:], g.NzbDigest) != len(j.NZBDigest) {
		return nil, fmt.Errorf("job %s: digest of length %d", g.ID, len(g.NzbDigest))
	}
	if g.FailureCode.Valid {
		j.Failure = &domain.Failure{Code: g.FailureCode.String, Message: g.FailureMessage.String}
	}
	if g.BackendRefJson.Valid {
		var ref domain.BackendRef
		if err := json.Unmarshal([]byte(g.BackendRefJson.String), &ref); err != nil {
			return nil, fmt.Errorf("job %s: backend ref: %w", g.ID, err)
		}
		j.BackendRef = &ref
	}
	for _, f := range files {
		j.Files = append(j.Files, domain.JobFile{RelPath: f.RelPath, Size: f.Size, ContentType: f.ContentType})
	}
	return j, nil
}

// load attaches files to a row; only Ready jobs have any.
func (s *Store) load(ctx context.Context, q *gen.Queries, g gen.Job) (*domain.Job, error) {
	var files []gen.JobFile
	if domain.Status(g.Status) == domain.StatusReady {
		var err error
		if files, err = q.JobFiles(ctx, g.ID); err != nil {
			return nil, err
		}
	}
	return toJob(g, files)
}

func (s *Store) loadAll(ctx context.Context, rows []gen.Job) ([]*domain.Job, error) {
	out := make([]*domain.Job, 0, len(rows))
	for _, g := range rows {
		j, err := s.load(ctx, s.r, g)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}

// tx runs fn in a write transaction (BEGIN IMMEDIATE).
func (s *Store) tx(ctx context.Context, fn func(q *gen.Queries) error) (err error) {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err := fn(s.w.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

func writeEvents(ctx context.Context, q *gen.Queries, j *domain.Job) error {
	ev := j.Events()
	if len(ev) == 0 {
		return nil
	}
	payload, err := json.Marshal(snapshot(j))
	if err != nil {
		return err
	}
	for _, e := range ev {
		if err := q.InsertEvent(ctx, gen.InsertEventParams{
			JobID: string(e.JobID), OwnerID: string(e.Owner), Type: string(e.Type), PayloadJson: string(payload), CreatedAt: ms(e.At),
		}); err != nil {
			return err
		}
	}
	return nil
}

// Create inserts the job, its NZB and its events. The duplicate check runs
// inside the write transaction, so two concurrent uploads of one NZB cannot
// both create a job; the partial unique index is the backstop.
func (s *Store) Create(ctx context.Context, j *domain.Job, nzb []byte) error {
	r, err := encodeRow(j)
	if err != nil {
		return err
	}
	var dup *domain.Job
	err = s.tx(ctx, func(q *gen.Queries) error {
		existing, err := q.LiveJobByDigest(ctx, gen.LiveJobByDigestParams{OwnerID: string(j.Owner), NzbDigest: j.NZBDigest[:]})
		switch {
		case err == nil:
			if dup, err = s.load(ctx, q, existing); err != nil {
				return err
			}
			return errDuplicate
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if err := q.InsertJob(ctx, gen.InsertJobParams{
			ID: string(j.ID), OwnerID: string(j.Owner), NzbName: j.NZBName, NzbDigest: j.NZBDigest[:], Status: string(j.Status),
			FailureCode: r.failureCode, FailureMessage: r.failureMessage, BackendRefJson: r.backendRef,
			CreatedAt: ms(j.CreatedAt), UpdatedAt: ms(j.UpdatedAt), ReadyAt: optMs(j.ReadyAt), FailedAt: optMs(j.FailedAt),
			ExpiresAt: optMs(j.ExpiresAt), NextCheckAt: optMs(j.NextCheckAt), Attempts: int64(j.Attempts),
			BackendRemovedAt: optMs(j.BackendRemovedAt),
		}); err != nil {
			return err
		}
		if err := q.InsertNZB(ctx, gen.InsertNZBParams{JobID: string(j.ID), Content: nzb}); err != nil {
			return err
		}
		return writeEvents(ctx, q, j)
	})
	if errors.Is(err, errDuplicate) {
		return &domain.DuplicateError{Existing: dup}
	}
	if err == nil {
		j.Version = 1
	}
	return err
}

var errDuplicate = errors.New("duplicate")

// Save writes a job back if its version is unchanged. It replaces the file
// catalogue and drops the NZB once the backend holds it or the job is
// terminal (UC-17).
func (s *Store) Save(ctx context.Context, j *domain.Job) error {
	r, err := encodeRow(j)
	if err != nil {
		return err
	}
	err = s.tx(ctx, func(q *gen.Queries) error {
		n, err := q.UpdateJob(ctx, gen.UpdateJobParams{
			Status: string(j.Status), FailureCode: r.failureCode, FailureMessage: r.failureMessage, BackendRefJson: r.backendRef,
			UpdatedAt: ms(j.UpdatedAt), ReadyAt: optMs(j.ReadyAt), FailedAt: optMs(j.FailedAt), ExpiresAt: optMs(j.ExpiresAt),
			NextCheckAt: optMs(j.NextCheckAt), Attempts: int64(j.Attempts), BackendRemovedAt: optMs(j.BackendRemovedAt),
			ID: string(j.ID), ExpectedVersion: j.Version,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return domain.ErrConflict
		}
		if err := q.DeleteJobFiles(ctx, string(j.ID)); err != nil {
			return err
		}
		if j.Status == domain.StatusReady {
			for _, f := range j.Files {
				if err := q.InsertJobFile(ctx, gen.InsertJobFileParams{JobID: string(j.ID), RelPath: f.RelPath, Size: f.Size, ContentType: f.ContentType}); err != nil {
					return err
				}
			}
		}
		if j.BackendRef != nil || !j.Status.InProgress() {
			if err := q.DeleteNZB(ctx, string(j.ID)); err != nil {
				return err
			}
		}
		return writeEvents(ctx, q, j)
	})
	if err == nil {
		j.Version++
	}
	return err
}

func (s *Store) Get(ctx context.Context, owner domain.OwnerID, id domain.JobID) (*domain.Job, error) {
	g, err := s.r.JobByOwner(ctx, gen.JobByOwnerParams{ID: string(id), OwnerID: string(owner)})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.r, g)
}

func (s *Store) Load(ctx context.Context, id domain.JobID) (*domain.Job, error) {
	g, err := s.r.JobByID(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.r, g)
}

func encodeCursor(j *domain.Job) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(ms(j.CreatedAt), 10) + "|" + string(j.ID)))
}

func decodeCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", domain.ErrBadCursor
	}
	ts, id, ok := strings.Cut(string(b), "|")
	n, err := strconv.ParseInt(ts, 10, 64)
	if !ok || err != nil || id == "" {
		return 0, "", domain.ErrBadCursor
	}
	return n, id, nil
}

func (s *Store) List(ctx context.Context, owner domain.OwnerID, q domain.ListQuery) ([]*domain.Job, string, error) {
	p := gen.ListJobsParams{OwnerID: string(owner), RowLimit: int64(q.Limit) + 1}
	if q.Status != "" {
		p.Status = string(q.Status)
	}
	if q.Cursor != "" {
		ts, id, err := decodeCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		p.CursorCreated, p.CursorID = ts, optStr(id)
	}
	rows, err := s.r.ListJobs(ctx, p)
	if err != nil {
		return nil, "", err
	}
	// One row more than the page tells whether a next page exists.
	more := len(rows) > q.Limit
	if more {
		rows = rows[:q.Limit]
	}
	jobs, err := s.loadAll(ctx, rows)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if more && len(jobs) > 0 {
		next = encodeCursor(jobs[len(jobs)-1])
	}
	return jobs, next, nil
}

func (s *Store) Counts(ctx context.Context, owner domain.OwnerID) (domain.Counts, error) {
	c, err := s.r.CountJobs(ctx, string(owner))
	if err != nil {
		return domain.Counts{}, err
	}
	return domain.Counts{Active: int(c.Active), Total: int(c.Total)}, nil
}

func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]*domain.Job, error) {
	rows, err := s.r.DueJobs(ctx, gen.DueJobsParams{NextCheckAt: optMs(now), Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	return s.loadAll(ctx, rows)
}

func (s *Store) Expired(ctx context.Context, now time.Time, limit int) ([]*domain.Job, error) {
	rows, err := s.r.ExpiredJobs(ctx, gen.ExpiredJobsParams{ExpiresAt: optMs(now), Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	return s.loadAll(ctx, rows)
}

func (s *Store) NZB(ctx context.Context, id domain.JobID) ([]byte, error) {
	b, err := s.r.NZBBlob(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNoNZB
	}
	return b, err
}

func (s *Store) KnownIDs(ctx context.Context) (map[domain.JobID]bool, error) {
	ids, err := s.r.KnownJobIDs(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[domain.JobID]bool, len(ids))
	for _, id := range ids {
		out[domain.JobID(id)] = true
	}
	return out, nil
}

func (s *Store) PurgeNZBs(ctx context.Context) (int64, error) { return s.w.PurgeNZBs(ctx) }
