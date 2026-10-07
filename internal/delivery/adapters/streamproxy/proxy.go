// Package streamproxy serves one job file from the backend to an HTTP client
// without touching local disk (specification UC-21, §10.2 Open, ADR 0003).
// Authorisation, signing and accounting happen before Serve is called.
package streamproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
	"github.com/feruzabad/mountenant/internal/platform/contenttype"
)

// File is a catalogued job file, as stored by Mountenant.
type File struct {
	Ref     domain.BackendRef
	RelPath string
	Size    int64
	ReadyAt time.Time // used as Last-Modified
}

// ETag is Mountenant's own strong validator for a file. Backend ETags are not
// usable (NzbDav has none; AltMount sends one only in PROPFIND).
func (f File) ETag() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", f.Ref.JobID, f.RelPath, f.Size)))
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// Opener is the subset of domain.Backend the proxy needs.
type Opener interface {
	Open(ctx context.Context, ref domain.BackendRef, relPath string, r *domain.ByteRange) (io.ReadCloser, error)
}

// Proxy streams files from a backend.
type Proxy struct {
	Backend Opener
	Logger  *slog.Logger
	// OnContentMissing is called when the backend no longer has the file, so
	// the job can be moved to Failed backend_content_missing.
	OnContentMissing func(File)
	// MaxResumes bounds transparent upstream resumes per response; 0 means 3.
	MaxResumes int

	resumes atomic.Int64
}

const copyBuffer = 128 << 10

// Serve answers a GET or HEAD for f. Errors before the first byte become
// status codes. If the backend body ends early, Serve reopens it at the
// current offset (up to MaxResumes times); if that fails too, it aborts the
// connection so the client sees a short body and resumes (specification §10.4).
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, f File) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	etag := f.ETag()
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", etag)
	h.Set("Last-Modified", f.ReadyAt.UTC().Format(http.TimeFormat))
	h.Set("Content-Type", contenttype.ForPath(f.RelPath))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(f.RelPath)}))
	h.Set("Cache-Control", "private, no-store")

	d := resolveRange(r.Header, f.Size, etag, f.ReadyAt)
	if d.unsatisfiable {
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", f.Size))
		h.Del("Content-Type")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	status, length := http.StatusOK, f.Size
	if d.rng != nil {
		status, length = http.StatusPartialContent, d.rng.Len()
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", d.rng.Start, d.rng.End, f.Size))
	}
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead || length == 0 {
		w.WriteHeader(status)
		return
	}

	body, err := p.Backend.Open(r.Context(), f.Ref, f.RelPath, d.rng)
	if err != nil {
		p.failBeforeBody(w, f, err)
		return
	}
	defer func() {
		if body != nil {
			body.Close()
		}
	}()

	w.WriteHeader(status)
	first := int64(0)
	if d.rng != nil {
		first = d.rng.Start
	}
	last := first + length - 1
	buf := make([]byte, copyBuffer)
	var sent int64
	resumes := 0
	for sent < length {
		nr, rerr := body.Read(buf[:min(int64(len(buf)), length-sent)])
		if nr > 0 {
			if _, werr := w.Write(buf[:nr]); werr != nil {
				panic(http.ErrAbortHandler) // client went away
			}
			sent += int64(nr)
		}
		if sent >= length || rerr == nil {
			continue
		}
		// The backend ended or broke the body early. AltMount 0.3.2 does this
		// intermittently with HTTP 200 and a full Content-Length
		// (docs/spike-backend-adapter.md). Reopen at the current offset with a
		// closed range so the client never notices.
		if r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		if resumes >= p.maxResumes() {
			p.log().Warn("download aborted mid-stream", "job", f.Ref.JobID, "path", f.RelPath, "sent", sent, "want", length, "resumes", resumes, "error", rerr)
			// Content-Length is already on the wire; resetting the connection
			// makes the short body visible instead of completing the response.
			panic(http.ErrAbortHandler)
		}
		resumes++
		p.resumes.Add(1)
		p.log().Info("resuming upstream stream", "job", f.Ref.JobID, "path", f.RelPath, "offset", first+sent, "attempt", resumes, "error", rerr)
		body.Close()
		body, err = p.Backend.Open(r.Context(), f.Ref, f.RelPath, &domain.ByteRange{Start: first + sent, End: last})
		if err != nil {
			p.log().Warn("download aborted: upstream resume failed", "job", f.Ref.JobID, "path", f.RelPath, "sent", sent, "error", err)
			panic(http.ErrAbortHandler)
		}
	}
}

// Resumes returns how many times an upstream stream was transparently resumed.
func (p *Proxy) Resumes() int64 { return p.resumes.Load() }

func (p *Proxy) maxResumes() int {
	if p.MaxResumes > 0 {
		return p.MaxResumes
	}
	return 3
}

func (p *Proxy) failBeforeBody(w http.ResponseWriter, f File, err error) {
	for _, k := range []string{"Content-Length", "Content-Range", "Content-Disposition", "ETag", "Last-Modified", "Accept-Ranges"} {
		w.Header().Del(k)
	}
	switch {
	case errors.Is(err, domain.ErrContentMissing):
		if p.OnContentMissing != nil {
			p.OnContentMissing(f)
		}
		http.Error(w, "content_missing", http.StatusGone)
	case errors.Is(err, context.Canceled):
		// Client went away; nothing to answer.
	default:
		p.log().Error("backend open failed", "job", f.Ref.JobID, "path", f.RelPath, "error", err)
		http.Error(w, "backend_unavailable", http.StatusBadGateway)
	}
}

func (p *Proxy) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
