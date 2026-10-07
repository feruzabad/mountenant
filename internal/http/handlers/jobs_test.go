package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/http/middleware"
	jobsapp "github.com/feruzabad/mountenant/internal/jobs/app"
	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

// fakeJobs records calls and answers from a fixed script.
type fakeJobs struct {
	submitted []jobsapp.SubmitInput
	body      string
	jobs      map[domain.JobID]*domain.Job
	submitErr error
	dup       bool
}

func (f *fakeJobs) Submit(_ context.Context, in jobsapp.SubmitInput) (jobsapp.SubmitResult, error) {
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return jobsapp.SubmitResult{}, err
	}
	f.body = string(b)
	f.submitted = append(f.submitted, in)
	if f.submitErr != nil {
		return jobsapp.SubmitResult{}, f.submitErr
	}
	j := domain.NewJob("0192f0e0-0000-7000-8000-000000000001", in.Owner, in.FileName, [32]byte{}, time.Unix(0, 0), domain.Retention{})
	return jobsapp.SubmitResult{Job: j, Duplicate: f.dup}, nil
}

func (f *fakeJobs) List(_ context.Context, owner domain.OwnerID, q domain.ListQuery) ([]*domain.Job, string, error) {
	if q.Cursor == "bad" {
		return nil, "", domain.ErrBadCursor
	}
	var out []*domain.Job
	for _, j := range f.jobs {
		if j.Owner == owner && (q.Status == "" || j.Status == q.Status) {
			out = append(out, j)
		}
	}
	return out, "next-page", nil
}

func (f *fakeJobs) Get(_ context.Context, owner domain.OwnerID, id domain.JobID) (*domain.Job, error) {
	j, ok := f.jobs[id]
	if !ok || j.Owner != owner {
		return nil, jobsapp.ErrNotFound
	}
	return j, nil
}

func (f *fakeJobs) Delete(ctx context.Context, owner domain.OwnerID, id domain.JobID) error {
	if _, err := f.Get(ctx, owner, id); err != nil {
		return err
	}
	delete(f.jobs, id)
	return nil
}

func multipartBody(t *testing.T, field, filename, content string) (string, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte(content))
	w.Close()
	return b.String(), w.FormDataContentType()
}

func (e *env) upload(t *testing.T, token, csrf, body, ctype string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(body))
	r = r.WithContext(middleware.WithClientIP(r.Context(), netip.MustParseAddr("203.0.113.9")))
	r.Header.Set("Content-Type", ctype)
	r.Header.Set("Origin", origin)
	r.Header.Set(CSRFHeader, csrf)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func (e *env) csrf(t *testing.T, token string) string {
	t.Helper()
	var c struct{ Token string }
	json.Unmarshal(e.do(req{method: "GET", path: "/api/v1/csrf", cookie: token}).Body.Bytes(), &c)
	return c.Token
}

func TestSubmitJob(t *testing.T) {
	e := newEnv(t, 100)
	token := e.login(t)
	csrf := e.csrf(t, token)

	body, ct := multipartBody(t, "nzb", "../Debian.nzb", "<nzb/>")
	rec := e.upload(t, token, csrf, body, ct)
	if rec.Code != 201 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Job       struct{ Id, Name, Status string }
		Duplicate bool
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Job.Status != "queued" || got.Job.Name != "Debian.nzb" || got.Duplicate {
		t.Fatalf("%s", rec.Body)
	}
	in := e.jobs.submitted[0]
	if in.Owner != "u1" || in.Limits.MaxActiveJobs != 5 || e.jobs.body != "<nzb/>" {
		t.Fatalf("input %+v body %q", in, e.jobs.body)
	}

	e.jobs.dup = true
	if rec := e.upload(t, token, csrf, body, ct); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
		t.Fatalf("duplicate: %d %s", rec.Code, rec.Body)
	}
	e.jobs.dup = false

	for name, c := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"invalid": {fmt.Errorf("%w: no files", domain.ErrInvalidNZB), 400, "invalid_nzb"},
		"large":   {domain.ErrNZBTooLarge, 413, "nzb_too_large"},
		"quota":   {domain.ErrQuotaExceeded, 422, "quota_exceeded"},
	} {
		e.jobs.submitErr = c.err
		rec := e.upload(t, token, csrf, body, ct)
		if rec.Code != c.status || code(t, rec) != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	e.jobs.submitErr = nil

	wrongField, ct2 := multipartBody(t, "file", "a.nzb", "<nzb/>")
	if rec := e.upload(t, token, csrf, wrongField, ct2); rec.Code != 400 {
		t.Errorf("missing nzb field: %d", rec.Code)
	}
	if rec := e.upload(t, token, "", body, ct); rec.Code != 403 {
		t.Errorf("upload without CSRF token: %d", rec.Code)
	}
	huge, ct3 := multipartBody(t, "nzb", "a.nzb", strings.Repeat("x", 2<<20))
	if rec := e.upload(t, token, csrf, huge, ct3); rec.Code != 413 {
		t.Errorf("body over MaxUploadBytes: %d %s", rec.Code, rec.Body)
	}
}

func TestListGetDeleteJobs(t *testing.T) {
	e := newEnv(t, 100)
	token := e.login(t)
	mine := domain.NewJob("0192f0e0-0000-7000-8000-00000000000a", "u1", "a.nzb", [32]byte{}, time.Unix(0, 0), domain.Retention{Ready: time.Hour})
	mine.MarkReady([]domain.JobFile{{RelPath: "dir/a.iso", Size: 3, ContentType: "application/x-iso9660-image"}}, 10, time.Unix(60, 0), domain.Retention{Ready: time.Hour})
	theirs := domain.NewJob("0192f0e0-0000-7000-8000-00000000000b", "u2", "b.nzb", [32]byte{}, time.Unix(0, 0), domain.Retention{})
	e.jobs.jobs = map[domain.JobID]*domain.Job{mine.ID: mine, theirs.ID: theirs}

	rec := e.do(req{method: "GET", path: "/api/v1/jobs?limit=10&status=ready", cookie: token})
	var page struct {
		Items []struct {
			Id    string
			Files []struct{ Path string }
		}
		NextCursor string
	}
	json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != 200 || len(page.Items) != 1 || page.Items[0].Files[0].Path != "dir/a.iso" || page.NextCursor != "next-page" {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"limit=0", "limit=101", "status=deleted", "cursor=bad"} {
		if rec := e.do(req{method: "GET", path: "/api/v1/jobs?" + q, cookie: token}); rec.Code != 400 {
			t.Errorf("%s: %d", q, rec.Code)
		}
	}

	rec = e.do(req{method: "GET", path: "/api/v1/jobs/" + string(mine.ID), cookie: token})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"readyAt":"1970-01-01T00:01:00Z"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(req{method: "GET", path: "/api/v1/jobs/" + string(theirs.ID), cookie: token}); rec.Code != 404 {
		t.Fatalf("foreign job: %d", rec.Code)
	}

	csrf := e.csrf(t, token)
	if rec := e.do(req{method: "DELETE", path: "/api/v1/jobs/" + string(theirs.ID), cookie: token, csrf: csrf}); rec.Code != 404 {
		t.Fatalf("delete foreign: %d", rec.Code)
	}
	if rec := e.do(req{method: "DELETE", path: "/api/v1/jobs/" + string(mine.ID), cookie: token, csrf: csrf}); rec.Code != 202 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
}
