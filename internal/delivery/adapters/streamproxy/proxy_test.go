package streamproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

type fakeOpener struct {
	data   []byte
	err    error
	failAt int64 // >0: body errors after this many bytes
	// failOpens limits failAt to the first n opens; 0 means every open.
	failOpens int
	// shortEOF ends the body with a clean io.EOF instead of an error,
	// like AltMount's truncated 200 responses.
	shortEOF bool
	calls    []*domain.ByteRange
}

func (f *fakeOpener) Open(_ context.Context, _ domain.BackendRef, _ string, r *domain.ByteRange) (io.ReadCloser, error) {
	f.calls = append(f.calls, r)
	if f.err != nil {
		return nil, f.err
	}
	b := f.data
	if r != nil {
		b = b[r.Start : r.End+1]
	}
	var rd io.Reader = bytes.NewReader(b)
	if f.failAt > 0 && (f.failOpens == 0 || len(f.calls) <= f.failOpens) {
		if f.shortEOF {
			rd = io.LimitReader(rd, f.failAt)
		} else {
			rd = io.MultiReader(io.LimitReader(rd, f.failAt), errReader{})
		}
	}
	return io.NopCloser(rd), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("upstream dropped") }

func newFile(size int64) File {
	return File{
		Ref:     domain.BackendRef{JobID: "0192f0e0-0000-7000-8000-000000000001"},
		RelPath: "disc/debian netinst.iso",
		Size:    size,
		ReadyAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
	}
}

func TestServeForwardsOnlyClosedRanges(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 100)
	op := &fakeOpener{data: data}
	p := &Proxy{Backend: op}
	f := newFile(int64(len(data)))

	cases := []struct {
		rangeHdr string
		status   int
		body     []byte
		backend  *domain.ByteRange
	}{
		{"", 200, data, nil},
		{"bytes=-10", 206, data[990:], &domain.ByteRange{Start: 990, End: 999}},
		{"bytes=995-99999", 206, data[995:], &domain.ByteRange{Start: 995, End: 999}},
		{"bytes=10-", 206, data[10:], &domain.ByteRange{Start: 10, End: 999}},
		{"bytes=0-1,5-6", 200, data, nil},
	}
	for _, c := range cases {
		op.calls = nil
		req := httptest.NewRequest(http.MethodGet, "/dl/x", nil)
		if c.rangeHdr != "" {
			req.Header.Set("Range", c.rangeHdr)
		}
		rec := httptest.NewRecorder()
		p.Serve(rec, req, f)
		if rec.Code != c.status || !bytes.Equal(rec.Body.Bytes(), c.body) {
			t.Fatalf("%q: got %d (%d bytes), want %d (%d bytes)", c.rangeHdr, rec.Code, rec.Body.Len(), c.status, len(c.body))
		}
		if len(op.calls) != 1 {
			t.Fatalf("%q: %d backend calls", c.rangeHdr, len(op.calls))
		}
		got := op.calls[0]
		if (got == nil) != (c.backend == nil) || (got != nil && *got != *c.backend) {
			t.Fatalf("%q: backend got range %v, want %v", c.rangeHdr, got, c.backend)
		}
	}
}

func TestServeUnsatisfiableAndHeadDoNotCallBackend(t *testing.T) {
	op := &fakeOpener{data: make([]byte, 100)}
	p := &Proxy{Backend: op}
	f := newFile(100)

	req := httptest.NewRequest(http.MethodGet, "/dl/x", nil)
	req.Header.Set("Range", "bytes=100-200")
	rec := httptest.NewRecorder()
	p.Serve(rec, req, f)
	if rec.Code != http.StatusRequestedRangeNotSatisfiable || rec.Header().Get("Content-Range") != "bytes */100" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Range"))
	}

	rec = httptest.NewRecorder()
	p.Serve(rec, httptest.NewRequest(http.MethodHead, "/dl/x", nil), f)
	if rec.Code != 200 || rec.Header().Get("Content-Length") != "100" {
		t.Fatalf("HEAD: %d %q", rec.Code, rec.Header().Get("Content-Length"))
	}
	if len(op.calls) != 0 {
		t.Fatalf("backend called %d times", len(op.calls))
	}
}

func TestServeHeaders(t *testing.T) {
	p := &Proxy{Backend: &fakeOpener{data: make([]byte, 10)}}
	f := newFile(10)
	rec := httptest.NewRecorder()
	p.Serve(rec, httptest.NewRequest(http.MethodGet, "/dl/x", nil), f)
	h := rec.Header()
	if h.Get("Content-Type") != "application/x-iso9660-image" {
		t.Errorf("Content-Type %q", h.Get("Content-Type"))
	}
	if !strings.Contains(h.Get("Content-Disposition"), "debian netinst.iso") {
		t.Errorf("Content-Disposition %q", h.Get("Content-Disposition"))
	}
	if h.Get("ETag") != f.ETag() || h.Get("Last-Modified") != "Wed, 07 Oct 2026 12:00:00 GMT" {
		t.Errorf("validators %q %q", h.Get("ETag"), h.Get("Last-Modified"))
	}
	if h.Get("Cache-Control") != "private, no-store" {
		t.Errorf("Cache-Control %q", h.Get("Cache-Control"))
	}
}

func TestServeErrorsBeforeFirstByte(t *testing.T) {
	var missing []File
	f := newFile(10)
	for _, c := range []struct {
		err  error
		code int
	}{
		{domain.ErrContentMissing, http.StatusGone},
		{domain.ErrBackendUnavailable, http.StatusBadGateway},
		{domain.ErrBadResponse, http.StatusBadGateway},
	} {
		p := &Proxy{Backend: &fakeOpener{err: c.err}, OnContentMissing: func(f File) { missing = append(missing, f) }}
		rec := httptest.NewRecorder()
		p.Serve(rec, httptest.NewRequest(http.MethodGet, "/dl/x", nil), f)
		if rec.Code != c.code || rec.Header().Get("Content-Range") != "" {
			t.Fatalf("%v: got %d", c.err, rec.Code)
		}
	}
	if len(missing) != 1 {
		t.Fatalf("OnContentMissing called %d times", len(missing))
	}
}

func TestServeAbortsMidStream(t *testing.T) {
	data := make([]byte, 1<<20)
	p := &Proxy{Backend: &fakeOpener{data: data, failAt: 1000}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Serve(w, r, newFile(int64(len(data))))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	if err == nil {
		t.Fatalf("read %d bytes without error; a truncated download must fail", n)
	}
	if n >= int64(len(data)) {
		t.Fatalf("read %d bytes", n)
	}
}

func TestServeResumesTruncatedUpstream(t *testing.T) {
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	for _, shortEOF := range []bool{true, false} {
		op := &fakeOpener{data: data, failAt: 100_000, failOpens: 2, shortEOF: shortEOF}
		p := &Proxy{Backend: op}
		req := httptest.NewRequest(http.MethodGet, "/dl/x", nil)
		req.Header.Set("Range", "bytes=1000-")
		rec := httptest.NewRecorder()
		p.Serve(rec, req, newFile(int64(len(data))))
		if rec.Code != 206 || !bytes.Equal(rec.Body.Bytes(), data[1000:]) {
			t.Fatalf("shortEOF=%v: got %d, %d bytes", shortEOF, rec.Code, rec.Body.Len())
		}
		if p.Resumes() != 2 || len(op.calls) != 3 {
			t.Fatalf("shortEOF=%v: resumes=%d opens=%d", shortEOF, p.Resumes(), len(op.calls))
		}
		// Each reopen asks for exactly the remaining bytes.
		want := []domain.ByteRange{{Start: 1000, End: 1<<20 - 1}, {Start: 101_000, End: 1<<20 - 1}, {Start: 201_000, End: 1<<20 - 1}}
		for i, c := range op.calls {
			if *c != want[i] {
				t.Fatalf("shortEOF=%v: open %d range %+v, want %+v", shortEOF, i, *c, want[i])
			}
		}
	}
}
