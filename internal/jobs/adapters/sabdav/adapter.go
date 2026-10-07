// Package sabdav implements the Backend port over the SABnzbd API (intake,
// status, delete) and WebDAV (listing, bytes), parameterised by a Profile.
// See specification §10 and docs/feasibility.md.
package sabdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"mountenant/internal/jobs/domain"
)

// Config is the backend section of Mountenant's configuration.
type Config struct {
	APIURL      string // e.g. http://altmount:8080 (no trailing slash)
	APIKey      string
	DavURL      string // e.g. http://altmount:8080
	DavUser     string
	DavPassword string
	Category    string // the namespace (ADR 0002)

	// MaxAttempts bounds retries of idempotent requests; 0 means 3.
	MaxAttempts int
	// RetryBase is the first backoff; 0 means 250ms.
	RetryBase time.Duration
}

// Adapter implements domain.Backend.
type Adapter struct {
	cfg     Config
	profile Profile
	http    *http.Client

	retries atomic.Int64 // retried attempts, exported for metrics
}

var _ domain.Backend = (*Adapter)(nil)

var categoryRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// New creates an adapter. client should carry no overall timeout because
// downloads are long-lived; per-call deadlines come from contexts.
func New(cfg Config, profile Profile, client *http.Client) (*Adapter, error) {
	if !categoryRE.MatchString(cfg.Category) {
		// NzbDav allows only letters, digits and dashes in categories.
		return nil, fmt.Errorf("sabdav: invalid category %q", cfg.Category)
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 250 * time.Millisecond
	}
	cfg.APIURL = strings.TrimSuffix(cfg.APIURL, "/")
	cfg.DavURL = strings.TrimSuffix(cfg.DavURL, "/")
	if client == nil {
		client = &http.Client{}
	}
	// Never follow redirects: AltMount answers traversal attempts with 307.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Adapter{cfg: cfg, profile: profile, http: &c}, nil
}

// Retries returns how many request attempts were retried so far.
func (a *Adapter) Retries() int64 { return a.retries.Load() }

// Profile returns the adapter's profile.
func (a *Adapter) Profile() Profile { return a.profile }

// retry runs fn until it succeeds, fails with a non-retryable error, or the
// attempts are used up. Only ErrBackendUnavailable is retried.
func (a *Adapter) retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < a.cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			a.retries.Add(1)
			backoff := a.cfg.RetryBase << (attempt - 1)
			backoff += time.Duration(rand.Int64N(int64(backoff)/2 + 1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		err = fn()
		if err == nil || !errors.Is(err, domain.ErrBackendUnavailable) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// namespaceURL is the WebDAV URL of the category directory, with trailing slash.
func (a *Adapter) namespaceURL() string {
	return a.cfg.DavURL + a.profile.ContentRoot + "/" + url.PathEscape(a.cfg.Category) + "/"
}

// jobDirURL is the WebDAV URL of a job directory, with trailing slash.
func (a *Adapter) jobDirURL(jobID string) string {
	return a.namespaceURL() + url.PathEscape(jobID) + "/"
}

// fileURL escapes every segment of relPath; RelPaths come from releases and
// may contain spaces, '#', '%', '?' or non-ASCII characters.
func (a *Adapter) fileURL(jobID, relPath string) (string, error) {
	if !validRelPath(relPath) {
		return "", fmt.Errorf("sabdav: invalid relative path %q", relPath)
	}
	segs := strings.Split(relPath, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return a.jobDirURL(jobID) + strings.Join(segs, "/"), nil
}

func validRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." {
			return false
		}
	}
	return true
}

// Ping implements domain.Backend.
func (a *Adapter) Ping(ctx context.Context) error {
	if _, err := a.sabCall(ctx, url.Values{"mode": {a.profile.ReadyMode}}, nil, ""); err != nil {
		return err
	}
	// The namespace directory may not exist before the first import (NzbDav);
	// a 404 still proves that WebDAV and its credentials work.
	_, _, err := a.propfind(ctx, a.namespaceURL(), "0")
	if err != nil && !errors.Is(err, domain.ErrContentMissing) {
		return err
	}
	return nil
}

// Submit implements domain.Backend.
func (a *Adapter) Submit(ctx context.Context, nzb []byte, jobID string) (domain.BackendRef, error) {
	nzo, err := a.addFile(ctx, nzb, jobID)
	if err != nil {
		return domain.BackendRef{}, err
	}
	return domain.BackendRef{JobID: jobID, NzoID: nzo}, nil
}

// Find implements domain.Backend. It searches the queue and the history for a
// row named after the job. Used before re-submitting (specification §10.4).
func (a *Adapter) Find(ctx context.Context, jobID string) (domain.BackendRef, bool, error) {
	q, err := a.queueSlots(ctx)
	if err != nil {
		return domain.BackendRef{}, false, err
	}
	for _, s := range q {
		if s.jobName() == jobID {
			return domain.BackendRef{JobID: jobID, NzoID: s.NzoID}, true, nil
		}
	}
	h, err := a.historySlots(ctx, "")
	if err != nil {
		return domain.BackendRef{}, false, err
	}
	for _, s := range h {
		if s.jobName() == jobID {
			return domain.BackendRef{JobID: jobID, NzoID: s.NzoID}, true, nil
		}
	}
	return domain.BackendRef{}, false, nil
}

// Status implements domain.Backend. Terminal history rows win; otherwise the
// queue is consulted. History is read twice around the queue so a job moving
// from queue to history in between is not reported as missing.
func (a *Adapter) Status(ctx context.Context, ref domain.BackendRef) (domain.BackendStatus, error) {
	first, err := a.historyStatus(ctx, ref)
	if err != nil || first.State == domain.BackendCompleted || first.State == domain.BackendFailed {
		return first, err
	}
	q, err := a.queueSlots(ctx)
	if err != nil {
		return domain.BackendStatus{}, err
	}
	for _, s := range q {
		if s.NzoID == ref.NzoID {
			return a.mapStatus(s), nil
		}
	}
	if first.State != domain.BackendNotFound {
		return first, nil
	}
	return a.historyStatus(ctx, ref)
}

func (a *Adapter) historyStatus(ctx context.Context, ref domain.BackendRef) (domain.BackendStatus, error) {
	h, err := a.historySlots(ctx, ref.NzoID)
	if err != nil {
		return domain.BackendStatus{}, err
	}
	if len(h) == 0 {
		return domain.BackendStatus{State: domain.BackendNotFound}, nil
	}
	// AltMount can hold duplicate rows for one nzo_id; a terminal row wins.
	best := a.mapStatus(h[0])
	for _, s := range h[1:] {
		st := a.mapStatus(s)
		if st.State == domain.BackendCompleted || st.State == domain.BackendFailed {
			best = st
		}
	}
	return best, nil
}

func (a *Adapter) mapStatus(s sabSlot) domain.BackendStatus {
	st, ok := a.profile.StatusMap[s.Status]
	if !ok {
		st = domain.BackendUnknown
	}
	out := domain.BackendStatus{State: st, Raw: s.Status}
	if st == domain.BackendFailed {
		out.FailMessage = s.failMessage()
	}
	return out
}

// ListFiles implements domain.Backend.
func (a *Adapter) ListFiles(ctx context.Context, ref domain.BackendRef) ([]domain.BackendFile, error) {
	dir := a.jobDirURL(ref.JobID)
	entries, _, err := a.propfind(ctx, dir, "infinity")
	if err != nil {
		return nil, err
	}
	u, _ := url.Parse(dir)
	prefix := u.Path // decoded, with trailing slash
	var files []domain.BackendFile
	for _, e := range entries {
		if e.IsDir {
			continue
		}
		rel, ok := strings.CutPrefix(e.Path, prefix)
		if !ok || !validRelPath(rel) {
			return nil, fmt.Errorf("%w: unexpected path %q outside %q", domain.ErrBadResponse, e.Path, prefix)
		}
		files = append(files, domain.BackendFile{RelPath: rel, Size: e.Size})
	}
	return files, nil
}

// Open implements domain.Backend. r must be nil or a closed range; the caller
// (the streaming proxy) has already resolved the client's Range header
// against the catalogued size (ADR 0003).
func (a *Adapter) Open(ctx context.Context, ref domain.BackendRef, relPath string, r *domain.ByteRange) (io.ReadCloser, error) {
	if r != nil && (r.Start < 0 || r.End < r.Start) {
		return nil, fmt.Errorf("sabdav: invalid range %d-%d", r.Start, r.End)
	}
	u, err := a.fileURL(ref.JobID, relPath)
	if err != nil {
		return nil, err
	}
	return a.davGet(ctx, u, r)
}

// Delete implements domain.Backend. Every step runs regardless of earlier
// failures; VerifyGone decides the outcome (ADR 0004).
func (a *Adapter) Delete(ctx context.Context, ref domain.BackendRef) error {
	var errs []error
	if ref.NzoID != "" {
		// Queue removal covers jobs deleted before their import finished.
		if q, err := a.queueSlots(ctx); err != nil {
			errs = append(errs, err)
		} else {
			for _, slot := range q {
				if slot.NzoID == ref.NzoID {
					if _, err := a.sabOnce(ctx, url.Values{"mode": {"queue"}, "name": {"delete"}, "value": {ref.NzoID}}); err != nil {
						errs = append(errs, err)
					}
				}
			}
		}
		// Mutations are not retried: NzbDav answers 500 for an id it no
		// longer knows, which is indistinguishable from a real failure.
		params := url.Values{"mode": {"history"}, "name": {"delete"}, "value": {ref.NzoID}}
		for k, v := range a.profile.DeleteParams {
			params[k] = v
		}
		if _, err := a.sabOnce(ctx, params); err != nil {
			errs = append(errs, err)
		}
	}
	// AltMount needs this to remove content; on NzbDav it is the fallback
	// for a directory left behind (requires webdav.enforce-readonly=false).
	if err := a.davDelete(ctx, a.jobDirURL(ref.JobID)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// VerifyGone implements domain.Backend.
func (a *Adapter) VerifyGone(ctx context.Context, ref domain.BackendRef) (bool, error) {
	if ref.NzoID != "" {
		h, err := a.historySlots(ctx, ref.NzoID)
		if err != nil {
			return false, err
		}
		if len(h) > 0 {
			return false, nil
		}
		q, err := a.queueSlots(ctx)
		if err != nil {
			return false, err
		}
		for _, s := range q {
			if s.NzoID == ref.NzoID {
				return false, nil
			}
		}
	}
	_, _, err := a.propfind(ctx, a.jobDirURL(ref.JobID), "0")
	switch {
	case errors.Is(err, domain.ErrContentMissing):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// ListNamespace implements domain.Backend.
func (a *Adapter) ListNamespace(ctx context.Context) (domain.NamespaceListing, error) {
	var out domain.NamespaceListing
	ns := a.namespaceURL()
	entries, now, err := a.propfind(ctx, ns, "1")
	switch {
	case errors.Is(err, domain.ErrContentMissing):
		// No import into the namespace yet.
	case err != nil:
		return out, err
	}
	out.BackendNow = now
	u, _ := url.Parse(ns)
	nsPath := strings.TrimSuffix(u.Path, "/")
	for _, e := range entries {
		if e.Path == nsPath || !e.IsDir {
			continue
		}
		out.Dirs = append(out.Dirs, domain.NamespaceEntry{Name: path.Base(e.Path), ModTime: e.ModTime})
	}
	h, err := a.historySlots(ctx, "")
	if err != nil {
		return out, err
	}
	for _, s := range h {
		e := domain.NamespaceEntry{Name: s.jobName(), NzoID: s.NzoID}
		if s.CompleteTime > 0 {
			e.ModTime = time.Unix(s.CompleteTime, 0)
		}
		out.History = append(out.History, e)
	}
	return out, nil
}
