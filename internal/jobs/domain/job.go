package domain

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// JobID is a UUIDv7 string; also the backend job directory name (ADR 0002).
type JobID string

// OwnerID is the owning user's ID. The Jobs context treats it as opaque.
type OwnerID string

// Status is the job lifecycle state (spec §4.2).
type Status string

const (
	StatusQueued    Status = "queued"
	StatusImporting Status = "importing"
	StatusReady     Status = "ready"
	StatusFailed    Status = "failed"
	StatusDeleted   Status = "deleted"
)

// Live reports whether the job counts as a duplicate target: Failed and
// Deleted jobs do not (spec §4.2).
func (s Status) Live() bool {
	return s == StatusQueued || s == StatusImporting || s == StatusReady
}

// InProgress reports whether the backend is still working on the job.
func (s Status) InProgress() bool { return s == StatusQueued || s == StatusImporting }

// Failure codes shown to users with the failure message.
const (
	FailBackendFailed  = "backend_failed"
	FailBackendReject  = "backend_rejected"
	FailImportTimeout  = "import_timeout"
	FailContentMissing = "backend_content_missing"
	FailTooManyFiles   = "too_many_files"
)

// Failure explains why a job failed.
type Failure struct {
	Code    string
	Message string
}

// JobFile is one catalogued file of a Ready job.
type JobFile struct {
	RelPath     string
	Size        int64
	ContentType string
}

// EventType names a domain event (spec §4.4); the names double as SSE event
// names (spec §6.4).
type EventType string

const (
	EventSubmitted EventType = "job.submitted"
	EventImporting EventType = "job.importing"
	EventReady     EventType = "job.ready"
	EventFailed    EventType = "job.failed"
	EventDeleted   EventType = "job.deleted"
)

// Event is raised by a transition and stored in the outbox in the same
// transaction as the job.
type Event struct {
	Type  EventType
	JobID JobID
	Owner OwnerID
	At    time.Time
}

// Retention holds the lifetimes from config (spec §4.2 RetentionPolicy).
type Retention struct {
	Ready         time.Duration // jobs.retention
	Failed        time.Duration // jobs.failedRetention
	ImportTimeout time.Duration // backend.importTimeout
}

// ErrInvalidTransition is returned for transitions the state machine forbids.
var ErrInvalidTransition = errors.New("invalid job transition")

// Job is the Jobs aggregate root.
type Job struct {
	ID         JobID
	Owner      OwnerID
	NZBName    string
	NZBDigest  [32]byte
	Status     Status
	Failure    *Failure
	BackendRef *BackendRef // nil until the backend accepted the NZB
	Files      []JobFile
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ReadyAt    time.Time
	FailedAt   time.Time
	ExpiresAt  time.Time // zero only when Deleted

	// Reconciler bookkeeping. NextCheckAt is zero when nothing is due:
	// Failed jobs and deleted jobs whose backend cleanup is verified.
	NextCheckAt      time.Time
	Attempts         int
	BackendRemovedAt time.Time

	// Version is the stored version this aggregate was loaded at; the
	// repository rejects a save if the row changed meanwhile.
	Version int64

	events []Event
}

// NewJob creates a Queued job. Its first expiry covers the import timeout
// plus the failed retention, so even a job the reconciler never sees again
// cannot live forever (spec §4.2).
func NewJob(id JobID, owner OwnerID, name string, digest [32]byte, now time.Time, r Retention) *Job {
	j := &Job{
		ID: id, Owner: owner, NZBName: SanitizeNZBName(name), NZBDigest: digest, Status: StatusQueued,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(r.ImportTimeout + r.Failed),
		NextCheckAt: now,
	}
	j.raise(EventSubmitted, now)
	return j
}

func (j *Job) raise(t EventType, now time.Time) {
	j.events = append(j.events, Event{Type: t, JobID: j.ID, Owner: j.Owner, At: now})
}

// Events returns and clears the events raised since the last call.
func (j *Job) Events() []Event {
	ev := j.events
	j.events = nil
	return ev
}

func (j *Job) transitionErr(to Status) error {
	return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, j.Status, to)
}

// Accepted records the backend's reference once the NZB was submitted.
func (j *Job) Accepted(ref BackendRef, now time.Time) error {
	if !j.Status.InProgress() {
		return j.transitionErr(j.Status)
	}
	j.BackendRef = &ref
	j.UpdatedAt = now
	return nil
}

// StartImport moves Queued to Importing.
func (j *Job) StartImport(now time.Time) error {
	if j.Status != StatusQueued {
		return j.transitionErr(StatusImporting)
	}
	j.Status, j.UpdatedAt = StatusImporting, now
	j.raise(EventImporting, now)
	return nil
}

// ErrNoFiles and ErrTooManyFiles are returned by MarkReady.
var (
	ErrNoFiles      = errors.New("job has no files")
	ErrTooManyFiles = errors.New("job has too many files")
	ErrBadRelPath   = errors.New("invalid file path")
)

// MarkReady catalogues the files and moves Queued or Importing to Ready.
func (j *Job) MarkReady(files []JobFile, maxFiles int, now time.Time, r Retention) error {
	if !j.Status.InProgress() {
		return j.transitionErr(StatusReady)
	}
	if len(files) == 0 {
		return ErrNoFiles
	}
	if len(files) > maxFiles {
		return fmt.Errorf("%w: %d > %d", ErrTooManyFiles, len(files), maxFiles)
	}
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		if !ValidRelPath(f.RelPath) || seen[f.RelPath] || f.Size < 0 {
			return fmt.Errorf("%w: %q", ErrBadRelPath, f.RelPath)
		}
		seen[f.RelPath] = true
	}
	j.Files = append([]JobFile(nil), files...)
	j.Status, j.ReadyAt, j.UpdatedAt, j.ExpiresAt = StatusReady, now, now, now.Add(r.Ready)
	j.raise(EventReady, now)
	return nil
}

// Fail moves Queued, Importing or Ready to Failed. Ready → Failed is how
// content that vanished from the backend is reported (spec §4.2).
func (j *Job) Fail(f Failure, now time.Time, r Retention) error {
	if j.Status != StatusQueued && j.Status != StatusImporting && j.Status != StatusReady {
		return j.transitionErr(StatusFailed)
	}
	j.Status, j.Failure, j.Files = StatusFailed, &f, nil
	j.FailedAt, j.UpdatedAt, j.ExpiresAt = now, now, now.Add(r.Failed)
	j.NextCheckAt, j.Attempts = time.Time{}, 0
	j.raise(EventFailed, now)
	return nil
}

// Delete moves any state except Deleted to Deleted. Backend cleanup follows
// asynchronously and is verified (ADR 0004).
func (j *Job) Delete(now time.Time) error {
	if j.Status == StatusDeleted {
		return j.transitionErr(StatusDeleted)
	}
	j.Status, j.Files, j.UpdatedAt, j.ExpiresAt = StatusDeleted, nil, now, time.Time{}
	j.NextCheckAt, j.Attempts = now, 0 // backend cleanup is due at once
	j.raise(EventDeleted, now)
	return nil
}

// ScheduleCheck sets when the reconciler looks at the job next.
func (j *Job) ScheduleCheck(at time.Time) { j.NextCheckAt = at }

// AttemptFailed counts a failed backend attempt and schedules a retry with
// exponential backoff from base, capped at max. It returns the new count.
func (j *Job) AttemptFailed(now time.Time, base, max time.Duration) int {
	j.Attempts++
	d := base << min(j.Attempts-1, 20)
	if d > max || d <= 0 {
		d = max
	}
	j.NextCheckAt = now.Add(d)
	return j.Attempts
}

// AttemptSucceeded resets the failure count.
func (j *Job) AttemptSucceeded() { j.Attempts = 0 }

// BackendRemoved records that VerifyGone confirmed the cleanup of a deleted
// job; nothing is due for it any more.
func (j *Job) BackendRemoved(now time.Time) error {
	if j.Status != StatusDeleted {
		return j.transitionErr(StatusDeleted)
	}
	j.BackendRemovedAt, j.NextCheckAt, j.Attempts = now, time.Time{}, 0
	return nil
}

// ImportTimedOut reports whether an in-progress job exceeded the import
// timeout.
func (j *Job) ImportTimedOut(now time.Time, r Retention) bool {
	return j.Status.InProgress() && now.Sub(j.CreatedAt) >= r.ImportTimeout
}

// Expired reports whether the job is past its expiry (UC-15).
func (j *Job) Expired(now time.Time) bool {
	return j.Status != StatusDeleted && !now.Before(j.ExpiresAt)
}

// File returns the catalogued file at relPath.
func (j *Job) File(relPath string) (JobFile, bool) {
	for _, f := range j.Files {
		if f.RelPath == relPath {
			return f, true
		}
	}
	return JobFile{}, false
}

// ValidRelPath accepts clean, relative, slash-separated paths without ".."
// segments (spec §4.2).
func ValidRelPath(p string) bool {
	if p == "" || len(p) > 4096 || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\\") || path.Clean(p) != p {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Limits are the owner's submission limits, as the Identity context
// supplies them.
type Limits struct {
	MaxActiveJobs int   // Queued + Importing
	MaxTotalJobs  int   // every job that is not Deleted
	MaxNZBBytes   int64 // decompressed XML; capped by the hard cap
}

// Counts are the owner's current jobs.
type Counts struct {
	Active int
	Total  int
}

// ErrQuotaExceeded is returned by CheckSubmission.
var ErrQuotaExceeded = errors.New("job quota exceeded")

// CheckSubmission is the SubmissionPolicy (spec §4.2): it runs before a job
// is created. The NZB size limit is enforced while reading the upload.
func CheckSubmission(l Limits, c Counts) error {
	if c.Active >= l.MaxActiveJobs {
		return fmt.Errorf("%w: %d active jobs (limit %d)", ErrQuotaExceeded, c.Active, l.MaxActiveJobs)
	}
	if c.Total >= l.MaxTotalJobs {
		return fmt.Errorf("%w: %d jobs (limit %d)", ErrQuotaExceeded, c.Total, l.MaxTotalJobs)
	}
	return nil
}

// EffectiveNZBLimit caps the owner's NZB limit by the hard cap.
func EffectiveNZBLimit(owner, hardCap int64) int64 { return min(owner, hardCap) }

// ParseNamespaceName extracts the job ID from a backend directory or
// history name: "<uuid>", or "<uuid> (n)" for a duplicate import. Anything
// else is not Mountenant's and must be left alone (UC-16).
func ParseNamespaceName(name string) (JobID, bool) {
	id, suffix, _ := strings.Cut(name, " ")
	if !isUUID(id) {
		return "", false
	}
	if suffix != "" {
		n, ok := strings.CutPrefix(suffix, "(")
		n, ok2 := strings.CutSuffix(n, ")")
		if !ok || !ok2 || n == "" || strings.Trim(n, "0123456789") != "" {
			return "", false
		}
	}
	return JobID(id), true
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
