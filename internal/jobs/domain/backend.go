// Package domain holds the Jobs bounded context: the Job aggregate, its
// policies and the ports it owns. It imports only the standard library.
package domain

import (
	"context"
	"errors"
	"io"
	"time"
)

// Backend is the port to the content backend (AltMount or NzbDav).
// It is the only contract the domain knows; see specification §4.5 and §10.
type Backend interface {
	// Ping checks that the backend API and WebDAV are reachable and that the
	// configured credentials are accepted. Used by /readyz.
	Ping(ctx context.Context) error

	// Submit hands a plain-XML NZB to the backend as job jobID.
	// It is not idempotent: callers retry only after Find reports nothing.
	Submit(ctx context.Context, nzb []byte, jobID string) (BackendRef, error)

	// Find looks a job up by its ID in the backend queue and history.
	// ok is false when the backend does not know the job.
	Find(ctx context.Context, jobID string) (ref BackendRef, ok bool, err error)

	// Status reports the backend's view of a job's import.
	Status(ctx context.Context, ref BackendRef) (BackendStatus, error)

	// ListFiles lists all files of a completed job, relative to its directory.
	ListFiles(ctx context.Context, ref BackendRef) ([]BackendFile, error)

	// Open streams one file. r is nil for the whole file, otherwise a closed
	// range inside the file; callers must never pass anything else (ADR 0003).
	Open(ctx context.Context, ref BackendRef, relPath string, r *ByteRange) (io.ReadCloser, error)

	// Delete asks the backend to remove the job and its content. Its result
	// does not decide success; VerifyGone does (ADR 0004).
	Delete(ctx context.Context, ref BackendRef) error

	// VerifyGone reports whether the job has disappeared from the backend
	// queue, history and WebDAV.
	VerifyGone(ctx context.Context, ref BackendRef) (bool, error)

	// ListNamespace lists everything in the namespace, for the orphan sweep.
	ListNamespace(ctx context.Context) (NamespaceListing, error)
}

// BackendRef identifies a job in the backend. Opaque outside the adapter.
type BackendRef struct {
	JobID string // Mountenant job ID; also the backend job directory name
	NzoID string // identifier assigned by the backend's SABnzbd API
}

// BackendState is the backend's view of a job, before domain mapping.
type BackendState int

const (
	BackendUnknown   BackendState = iota // unrecognised backend status
	BackendQueued                        // waiting in the backend queue
	BackendImporting                     // import in progress
	BackendCompleted                     // import finished; content should be listable
	BackendFailed                        // import failed; see FailMessage
	BackendNotFound                      // neither in queue nor in history
)

func (s BackendState) String() string {
	switch s {
	case BackendQueued:
		return "queued"
	case BackendImporting:
		return "importing"
	case BackendCompleted:
		return "completed"
	case BackendFailed:
		return "failed"
	case BackendNotFound:
		return "not_found"
	default:
		return "unknown"
	}
}

// BackendStatus is the result of Backend.Status.
type BackendStatus struct {
	State       BackendState
	Raw         string // backend status string, for logs
	FailMessage string // set when State is BackendFailed
}

// BackendFile is one file in a completed job.
type BackendFile struct {
	RelPath string // slash-separated, relative to the job directory, no ".."
	Size    int64
}

// ByteRange is a closed byte range, both ends inclusive.
type ByteRange struct {
	Start, End int64
}

// Len returns the number of bytes in the range.
func (r ByteRange) Len() int64 { return r.End - r.Start + 1 }

// NamespaceListing is everything the backend holds in the namespace.
type NamespaceListing struct {
	BackendNow time.Time        // backend clock (Date header); backend timestamps are compared only to this
	Dirs       []NamespaceEntry // job directories
	History    []NamespaceEntry // SABnzbd history rows in the namespace category
}

// NamespaceEntry is one directory or history row in the namespace.
type NamespaceEntry struct {
	Name    string    // directory name or history name, e.g. "<jobId>" or "<jobId> (2)"
	NzoID   string    // history rows only
	ModTime time.Time // backend clock
}

var (
	// ErrBackendUnavailable means the backend could not be reached or answered
	// with a server error. Retryable.
	ErrBackendUnavailable = errors.New("backend unavailable")
	// ErrBackendRejected means the backend refused a request (bad NZB, unknown
	// category, wrong credentials). Not retryable without a config change.
	ErrBackendRejected = errors.New("backend rejected request")
	// ErrContentMissing means the job directory or file is gone (WebDAV 404).
	ErrContentMissing = errors.New("backend content missing")
	// ErrBadResponse means the backend answered with something that does not
	// match the request (wrong range, wrong length).
	ErrBadResponse = errors.New("backend response invalid")
)
