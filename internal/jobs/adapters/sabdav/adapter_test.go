package sabdav

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"mountenant/internal/jobs/domain"
)

// fakeBackend imitates one product closely enough to exercise the profile
// differences observed in docs/feasibility.md: API path, error style, href
// form, delete semantics. It rejects any Range header that is not a closed
// in-bounds range, the way the contract tests must (ADR 0003).
type fakeBackend struct {
	t       *testing.T
	profile Profile
	key     string

	mu        sync.Mutex
	nextID    int
	queue     map[string]*fakeJob // nzo -> job
	history   map[string]*fakeJob
	content   map[string]map[string][]byte // jobName -> relPath -> bytes
	fail500   int                          // fail the next N GETs with 500
	badRanges []string
}

type fakeJob struct {
	nzo, name, status, fail string
}

const (
	fakeKey  = "k"
	fakeCat  = "mountenant"
	fakeUser = "u"
	fakePass = "p"
)

func newFake(t *testing.T, p Profile) (*fakeBackend, *httptest.Server) {
	f := &fakeBackend{t: t, profile: p, key: fakeKey,
		queue: map[string]*fakeJob{}, history: map[string]*fakeJob{}, content: map[string]map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == f.profile.APIPath {
		f.sab(w, r)
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != fakeUser || p != fakePass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.dav(w, r)
}

func (f *fakeBackend) sabErr(w http.ResponseWriter, code int, msg string) {
	if f.profile.Name == "altmount" {
		code = 200 // AltMount reports every SAB error as HTTP 200
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"status": false, "error": msg})
}

func (f *fakeBackend) sab(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("apikey") != f.key {
		f.sabErr(w, 401, "API Key Incorrect")
		return
	}
	switch q.Get("mode") {
	case "version", "queue":
		if q.Get("name") == "delete" {
			delete(f.queue, q.Get("value"))
			json.NewEncoder(w).Encode(map[string]any{"status": true})
			return
		}
		var slots []map[string]any
		for _, j := range f.queue {
			slots = append(slots, map[string]any{"nzo_id": j.nzo, "filename": j.name + ".nzb", "cat": fakeCat, "status": j.status})
		}
		json.NewEncoder(w).Encode(map[string]any{"status": true, "version": "4.5.0", "queue": map[string]any{"slots": slots}})
	case "history":
		if q.Get("name") == "delete" {
			j, ok := f.history[q.Get("value")]
			if !ok {
				if f.profile.Name == "nzbdav" {
					f.sabErr(w, 500, "The database operation was expected to affect 1 row(s)")
					return
				}
			} else {
				delete(f.history, j.nzo)
				if q.Get("del_completed_files") == "1" && f.profile.Name == "nzbdav" {
					delete(f.content, j.name)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"status": true})
			return
		}
		var slots []map[string]any
		for _, j := range f.history {
			if ids := q.Get("nzo_ids"); ids != "" && ids != j.nzo {
				continue
			}
			slots = append(slots, map[string]any{"nzo_id": j.nzo, "name": j.name, "category": fakeCat, "status": j.status, "fail_message": j.fail})
		}
		// AltMount omits "status" on history responses.
		json.NewEncoder(w).Encode(map[string]any{"history": map[string]any{"slots": slots}})
	case "addfile":
		if q.Get("cat") != fakeCat && f.profile.Name == "altmount" {
			f.sabErr(w, 200, "invalid category")
			return
		}
		_, hdr, err := r.FormFile("name")
		if err != nil {
			f.sabErr(w, 400, "No NZB file provided")
			return
		}
		f.nextID++
		nzo := fmt.Sprintf("nzo-%d", f.nextID)
		if f.profile.Name == "altmount" {
			nzo = q.Get("nzbname")
		}
		name := strings.TrimSuffix(hdr.Filename, ".nzb")
		f.queue[nzo] = &fakeJob{nzo: nzo, name: name, status: "Queued"}
		json.NewEncoder(w).Encode(map[string]any{"status": true, "nzo_ids": []string{nzo}})
	default:
		f.sabErr(w, 400, "unknown mode")
	}
}

// complete moves a queued job to history with content.
func (f *fakeBackend) complete(nzo string, files map[string][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.queue[nzo]
	delete(f.queue, nzo)
	j.status = "Completed"
	f.history[nzo] = j
	f.content[j.name] = files
}

var closedRange = regexp.MustCompile(`^bytes=(\d+)-(\d+)$`)

func (f *fakeBackend) dav(w http.ResponseWriter, r *http.Request) {
	root := f.profile.ContentRoot + "/" + fakeCat
	p := strings.TrimSuffix(r.URL.Path, "/")
	rest, ok := strings.CutPrefix(p, root)
	if !ok {
		w.WriteHeader(404)
		return
	}
	rest = strings.TrimPrefix(rest, "/")
	job, rel, _ := strings.Cut(rest, "/")

	switch r.Method {
	case "PROPFIND":
		f.propfind(w, r, root, job, rel)
	case http.MethodDelete:
		if _, ok := f.content[job]; !ok || rel != "" {
			w.WriteHeader(404)
			return
		}
		delete(f.content, job)
		w.WriteHeader(204)
	case http.MethodGet:
		if f.fail500 > 0 {
			f.fail500--
			w.WriteHeader(500)
			return
		}
		data, ok := f.content[job][rel]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if rh := r.Header.Get("Range"); rh != "" {
			m := closedRange.FindStringSubmatch(rh)
			var a, b int
			if m != nil {
				fmt.Sscan(m[1], &a)
				fmt.Sscan(m[2], &b)
			}
			if m == nil || a > b || b >= len(data) {
				f.badRanges = append(f.badRanges, rh)
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			w.Header().Set("Content-Length", fmt.Sprint(b-a+1))
			w.WriteHeader(206)
			w.Write(data[a : b+1])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data)
	default:
		w.WriteHeader(405)
	}
}

func (f *fakeBackend) propfind(w http.ResponseWriter, r *http.Request, root, job, rel string) {
	type entry struct {
		path string
		dir  bool
		size int
	}
	var out []entry
	switch {
	case job == "":
		out = append(out, entry{root, true, 0})
		for name := range f.content {
			out = append(out, entry{root + "/" + name, true, 0})
		}
	case rel == "":
		files, ok := f.content[job]
		if !ok {
			w.WriteHeader(404)
			return
		}
		out = append(out, entry{root + "/" + job, true, 0})
		if r.Header.Get("Depth") != "0" {
			for name, b := range files {
				out = append(out, entry{root + "/" + job + "/" + name, false, len(b)})
			}
		}
	default:
		w.WriteHeader(404)
		return
	}
	w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	w.WriteHeader(207)
	fmt.Fprint(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:">`)
	for _, e := range out {
		href := (&url.URL{Path: e.path}).EscapedPath()
		if f.profile.Name == "nzbdav" {
			href = "http://localhost:8080" + href // absolute, no trailing slash
		} else if e.dir {
			href += "/"
		}
		rt := ""
		if e.dir {
			rt = "<D:collection/>"
		}
		fmt.Fprintf(w, `<D:response><D:href>%s</D:href><D:propstat><D:prop><D:resourcetype>%s</D:resourcetype><D:getcontentlength>%d</D:getcontentlength><D:getlastmodified>Wed, 07 Oct 2026 12:00:00 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, href, rt, e.size)
	}
	fmt.Fprint(w, `</D:multistatus>`)
}

func newAdapter(t *testing.T, p Profile, srvURL string) *Adapter {
	a, err := New(Config{
		APIURL: srvURL, APIKey: fakeKey, DavURL: srvURL, DavUser: fakeUser, DavPassword: fakePass,
		Category: fakeCat, RetryBase: time.Millisecond,
	}, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLifecycle(t *testing.T) {
	for _, p := range []Profile{AltMount, NzbDav} {
		t.Run(p.Name, func(t *testing.T) {
			ctx := context.Background()
			f, srv := newFake(t, p)
			a := newAdapter(t, p, srv.URL)
			const job = "0192f0e0-0000-7000-8000-000000000001"

			if err := a.Ping(ctx); err != nil {
				t.Fatalf("Ping: %v", err)
			}
			ref, err := a.Submit(ctx, []byte("<nzb/>"), job)
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if found, ok, err := a.Find(ctx, job); err != nil || !ok || found != ref {
				t.Fatalf("Find = %+v %v %v, want %+v", found, ok, err, ref)
			}
			if st, err := a.Status(ctx, ref); err != nil || st.State != domain.BackendQueued {
				t.Fatalf("Status = %+v %v", st, err)
			}

			iso := []byte(strings.Repeat("x", 1000))
			f.complete(ref.NzoID, map[string][]byte{"debian netinst #1.iso": iso})
			if st, err := a.Status(ctx, ref); err != nil || st.State != domain.BackendCompleted {
				t.Fatalf("Status = %+v %v", st, err)
			}
			files, err := a.ListFiles(ctx, ref)
			if err != nil || len(files) != 1 || files[0].RelPath != "debian netinst #1.iso" || files[0].Size != 1000 {
				t.Fatalf("ListFiles = %+v %v", files, err)
			}

			rc, err := a.Open(ctx, ref, files[0].RelPath, &domain.ByteRange{Start: 990, End: 999})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != "xxxxxxxxxx" {
				t.Fatalf("Open read %q", b)
			}

			ns, err := a.ListNamespace(ctx)
			if err != nil || len(ns.Dirs) != 1 || ns.Dirs[0].Name != job || len(ns.History) != 1 || ns.BackendNow.IsZero() {
				t.Fatalf("ListNamespace = %+v %v", ns, err)
			}

			if err := a.Delete(ctx, ref); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if gone, err := a.VerifyGone(ctx, ref); err != nil || !gone {
				t.Fatalf("VerifyGone = %v %v", gone, err)
			}
			// A second delete may fail (NzbDav: 500) but must not hide state.
			_ = a.Delete(ctx, ref)
			if gone, err := a.VerifyGone(ctx, ref); err != nil || !gone {
				t.Fatalf("VerifyGone after second delete = %v %v", gone, err)
			}
			if _, ok, _ := a.Find(ctx, job); ok {
				t.Fatal("Find after delete still finds the job")
			}
			if len(f.badRanges) > 0 {
				t.Fatalf("backend received unsafe ranges: %v", f.badRanges)
			}
		})
	}
}

func TestVerifyGoneSeesLeftoverContent(t *testing.T) {
	// The history row is gone but the directory remains: not gone.
	f, srv := newFake(t, NzbDav)
	a := newAdapter(t, NzbDav, srv.URL)
	ref, _ := a.Submit(context.Background(), []byte("<nzb/>"), "job-1")
	f.complete(ref.NzoID, map[string][]byte{"a.bin": []byte("a")})
	f.mu.Lock()
	delete(f.history, ref.NzoID)
	f.mu.Unlock()
	if gone, err := a.VerifyGone(context.Background(), ref); err != nil || gone {
		t.Fatalf("VerifyGone = %v %v, want false", gone, err)
	}
	// Delete falls back to WebDAV DELETE.
	_ = a.Delete(context.Background(), ref)
	if gone, err := a.VerifyGone(context.Background(), ref); err != nil || !gone {
		t.Fatalf("VerifyGone after delete = %v %v", gone, err)
	}
}

func TestErrorStyles(t *testing.T) {
	for _, p := range []Profile{AltMount, NzbDav} {
		t.Run(p.Name, func(t *testing.T) {
			f, srv := newFake(t, p)
			f.key = "other"
			a := newAdapter(t, p, srv.URL)
			if err := a.Ping(context.Background()); !errors.Is(err, domain.ErrBackendRejected) {
				t.Fatalf("Ping with wrong key = %v, want ErrBackendRejected", err)
			}
		})
	}
}

func TestOpenRetriesTransient500(t *testing.T) {
	f, srv := newFake(t, NzbDav)
	a := newAdapter(t, NzbDav, srv.URL)
	ref, _ := a.Submit(context.Background(), []byte("<nzb/>"), "job-1")
	f.complete(ref.NzoID, map[string][]byte{"a.bin": []byte("abc")})
	f.fail500 = 2
	rc, err := a.Open(context.Background(), ref, "a.bin", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rc.Close()
	if a.Retries() != 2 {
		t.Fatalf("retries = %d, want 2", a.Retries())
	}
	f.fail500 = 5
	if _, err := a.Open(context.Background(), ref, "a.bin", nil); !errors.Is(err, domain.ErrBackendUnavailable) {
		t.Fatalf("Open after persistent 500 = %v", err)
	}
}

func TestOpenRejectsUnsafeInput(t *testing.T) {
	_, srv := newFake(t, AltMount)
	a := newAdapter(t, AltMount, srv.URL)
	ref := domain.BackendRef{JobID: "j", NzoID: "j"}
	for _, p := range []string{"../x", "a/../../b", "/etc/passwd", "a//b", ""} {
		if _, err := a.Open(context.Background(), ref, p, nil); err == nil {
			t.Errorf("Open(%q) succeeded", p)
		}
	}
	if _, err := a.Open(context.Background(), ref, "a", &domain.ByteRange{Start: -1, End: 5}); err == nil {
		t.Error("Open with negative start succeeded")
	}
}

func TestParseResponseHrefForms(t *testing.T) {
	ok := []davPropstat{{Status: "HTTP/1.1 200 OK"}}
	for _, href := range []string{
		"/webdav/complete/mountenant/j/a%20b.iso",
		"http://localhost:8080/webdav/complete/mountenant/j/a%20b.iso",
	} {
		e, found, err := parseResponse(href, ok)
		if err != nil || !found || e.Path != "/webdav/complete/mountenant/j/a b.iso" {
			t.Errorf("%s -> %+v %v %v", href, e, found, err)
		}
	}
}
