//go:build live

// Live contract test against real AltMount and NzbDav instances.
//
//	go test -tags live -v -timeout 30m ./internal/jobs/adapters/sabdav -run TestLive
//
// Environment (a backend is skipped when its *_API_URL is unset):
//
//	MT_ALTMOUNT_API_URL, MT_ALTMOUNT_API_KEY, MT_ALTMOUNT_DAV_USER, MT_ALTMOUNT_DAV_PASS
//	MT_NZBDAV_API_URL,   MT_NZBDAV_API_KEY,   MT_NZBDAV_DAV_USER,   MT_NZBDAV_DAV_PASS
//	MT_LIVE_CATEGORY     namespace category (default "mountenant")
//	MT_LIVE_NZB          NZB that imports successfully (required)
//	MT_LIVE_NZB_FAIL     optional NZBs that must fail, separated by ":"
//	MT_LIVE_REF          optional local copy of the imported file, for byte comparison
//	MT_LIVE_SHA256       optional expected SHA-256 of the imported file
//	MT_LIVE_FULL=n       also download the whole file through the proxy n times
package sabdav_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/delivery/adapters/streamproxy"
	"github.com/feruzabad/mountenant/internal/jobs/adapters/sabdav"
	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

func uuid() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func liveAdapter(t *testing.T, p sabdav.Profile, prefix string) *sabdav.Adapter {
	api := os.Getenv(prefix + "_API_URL")
	if api == "" {
		t.Skipf("%s_API_URL not set", prefix)
	}
	cat := os.Getenv("MT_LIVE_CATEGORY")
	if cat == "" {
		cat = "mountenant"
	}
	a, err := sabdav.New(sabdav.Config{
		APIURL: api, APIKey: os.Getenv(prefix + "_API_KEY"),
		DavURL: api, DavUser: os.Getenv(prefix + "_DAV_USER"), DavPassword: os.Getenv(prefix + "_DAV_PASS"),
		Category: cat,
	}, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// waitTerminal polls Status and logs every state change.
func waitTerminal(t *testing.T, a *sabdav.Adapter, ref domain.BackendRef, timeout time.Duration) domain.BackendStatus {
	t.Helper()
	ctx := context.Background()
	start := time.Now()
	last := ""
	for time.Since(start) < timeout {
		st, err := a.Status(ctx, ref)
		if err != nil {
			t.Logf("  %6.1fs status error: %v", time.Since(start).Seconds(), err)
		} else {
			cur := fmt.Sprintf("%s (raw %q)", st.State, st.Raw)
			if cur != last {
				t.Logf("  %6.1fs %s", time.Since(start).Seconds(), cur)
				last = cur
			}
			if st.State == domain.BackendCompleted || st.State == domain.BackendFailed {
				return st
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("job %s not terminal after %s", ref.JobID, timeout)
	return domain.BackendStatus{}
}

func waitGone(t *testing.T, a *sabdav.Adapter, ref domain.BackendRef) {
	t.Helper()
	for i := 0; i < 30; i++ {
		gone, err := a.VerifyGone(context.Background(), ref)
		if err == nil && gone {
			t.Logf("  verified gone after %d check(s)", i+1)
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("job %s not gone", ref.JobID)
}

func TestLive(t *testing.T) {
	nzbPath := os.Getenv("MT_LIVE_NZB")
	if nzbPath == "" {
		t.Skip("MT_LIVE_NZB not set")
	}
	nzb, err := os.ReadFile(nzbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p      sabdav.Profile
		prefix string
	}{{sabdav.AltMount, "MT_ALTMOUNT"}, {sabdav.NzbDav, "MT_NZBDAV"}} {
		t.Run(c.p.Name, func(t *testing.T) {
			a := liveAdapter(t, c.p, c.prefix)
			liveBackend(t, a, nzb)
		})
	}
}

func liveBackend(t *testing.T, a *sabdav.Adapter, nzb []byte) {
	ctx := context.Background()
	if err := a.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	t.Log("Ping ok")

	jobID := uuid()
	if _, ok, err := a.Find(ctx, jobID); err != nil || ok {
		t.Fatalf("Find(unknown) = %v %v", ok, err)
	}

	t.Logf("Submit job %s", jobID)
	ref, err := a.Submit(ctx, nzb, jobID)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	t.Logf("  nzo_id %s", ref.NzoID)
	if found, ok, err := a.Find(ctx, jobID); err != nil || !ok || found.NzoID != ref.NzoID {
		t.Fatalf("Find(job) = %+v %v %v", found, ok, err)
	}
	t.Log("Find(job) ok")

	st := waitTerminal(t, a, ref, 5*time.Minute)
	if st.State != domain.BackendCompleted {
		t.Fatalf("import failed: %s", st.FailMessage)
	}

	files, err := a.ListFiles(ctx, ref)
	if err != nil || len(files) == 0 {
		t.Fatalf("ListFiles = %v %v", files, err)
	}
	big := files[0]
	for _, f := range files {
		t.Logf("  file %q %d bytes", f.RelPath, f.Size)
		if f.Size > big.Size {
			big = f
		}
	}

	ns, err := a.ListNamespace(ctx)
	if err != nil {
		t.Fatalf("ListNamespace: %v", err)
	}
	inNS := false
	for _, d := range ns.Dirs {
		inNS = inNS || d.Name == jobID
	}
	t.Logf("ListNamespace: %d dirs, %d history rows, backend clock %s (local %s); job listed: %v",
		len(ns.Dirs), len(ns.History), ns.BackendNow.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), inNS)
	if !inNS {
		t.Fatal("job directory missing from namespace listing")
	}

	liveProxy(t, a, ref, big)

	t.Log("Delete")
	if err := a.Delete(ctx, ref); err != nil {
		t.Logf("  delete reported: %v (outcome decided by VerifyGone)", err)
	}
	waitGone(t, a, ref)
	if err := a.Delete(ctx, ref); err != nil {
		t.Logf("  second delete reported: %v", err)
	}
	waitGone(t, a, ref)

	for _, p := range strings.Split(os.Getenv("MT_LIVE_NZB_FAIL"), ":") {
		if p == "" {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		id := uuid()
		t.Logf("Submit failing NZB %s as %s", p, id)
		r, err := a.Submit(ctx, b, id)
		if err != nil {
			t.Logf("  rejected at submit: %v", err)
			continue
		}
		st := waitTerminal(t, a, r, 5*time.Minute)
		if st.State != domain.BackendFailed {
			t.Errorf("  expected failure, got %s", st.State)
		}
		t.Logf("  fail_message: %s", st.FailMessage)
		_ = a.Delete(ctx, r)
		waitGone(t, a, r)
	}
	t.Logf("adapter retries: %d", a.Retries())
}

func liveProxy(t *testing.T, a *sabdav.Adapter, ref domain.BackendRef, bf domain.BackendFile) {
	ctx := context.Background()
	file := streamproxy.File{Ref: ref, RelPath: bf.RelPath, Size: bf.Size, ReadyAt: time.Now().UTC().Truncate(time.Second)}
	missing := 0
	proxy := &streamproxy.Proxy{Backend: a, OnContentMissing: func(streamproxy.File) { missing++ }}
	served := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { served <- struct{}{} }()
		proxy.Serve(w, r, file)
	}))
	defer srv.Close()

	var ref0 *os.File
	if p := os.Getenv("MT_LIVE_REF"); p != "" {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		ref0 = f
	}
	expect := func(off, n int64) []byte {
		if ref0 != nil {
			b := make([]byte, n)
			if _, err := ref0.ReadAt(b, off); err != nil {
				t.Fatal(err)
			}
			return b
		}
		rc, err := a.Open(ctx, ref, bf.RelPath, &domain.ByteRange{Start: off, End: off + n - 1})
		if err != nil {
			t.Fatalf("direct Open: %v", err)
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		return b
	}
	get := func(hdr map[string]string) (*http.Response, []byte) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy GET: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		<-served
		return resp, b
	}
	size := bf.Size
	etag := file.ETag()

	t.Log("Range matrix through the proxy (client header -> response):")
	matrix := []struct {
		name   string
		hdr    map[string]string
		status int
		off, n int64 // expected body slice; n = 0 means none
	}{
		{"suffix bytes=-1024", map[string]string{"Range": "bytes=-1024"}, 206, size - 1024, 1024},
		{"end past EOF", map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", size-10, size+100)}, 206, size - 10, 10},
		{"open-ended", map[string]string{"Range": fmt.Sprintf("bytes=%d-", size-1000)}, 206, size - 1000, 1000},
		{"closed", map[string]string{"Range": "bytes=32768-33791"}, 206, 32768, 1024},
		{"unsatisfiable", map[string]string{"Range": fmt.Sprintf("bytes=%d-", size)}, 416, 0, 0},
		{"If-Range matching ETag", map[string]string{"Range": "bytes=0-9", "If-Range": etag}, 206, 0, 10},
		{"If-Range stale ETag", map[string]string{"Range": "bytes=0-9", "If-Range": `"stale"`}, 200, 0, -1},
		{"multi-range", map[string]string{"Range": "bytes=0-9,20-29"}, 200, 0, -1},
	}
	for _, m := range matrix {
		resp, body := get(m.hdr)
		ok := resp.StatusCode == m.status
		switch {
		case m.n > 0:
			ok = ok && bytes.Equal(body, expect(m.off, m.n))
		case m.n < 0: // whole file; only check length to avoid a second full read
			ok = ok && int64(len(body)) == size
		}
		t.Logf("  %-24s -> %d %-28s %d bytes ok=%v", m.name, resp.StatusCode, resp.Header.Get("Content-Range"), len(body), ok)
		if !ok {
			t.Errorf("range case %q failed", m.name)
		}
	}

	// The cases above destroyed files on AltMount 0.3.2 when sent directly.
	files, err := a.ListFiles(ctx, ref)
	if err != nil || len(files) == 0 {
		t.Fatalf("file gone after range matrix: %v %v", files, err)
	}
	if b := expect(0, 10); len(b) != 10 {
		t.Fatal("file unreadable after range matrix")
	}
	t.Log("  file still listed and readable after the matrix")

	t.Log("20 random ranges through the proxy (4 parallel):")
	type res struct {
		ok  bool
		dur time.Duration
	}
	results := make(chan res, 20)
	sem := make(chan struct{}, 4)
	for i := 0; i < 20; i++ {
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			n := int64(64 << 10)
			if mrand.IntN(2) == 0 {
				n = 1 << 20
			}
			off := mrand.Int64N(size - n)
			start := time.Now()
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- res{}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			<-served
			want := expect(off, n)
			results <- res{resp.StatusCode == 206 && bytes.Equal(b, want), time.Since(start)}
		}()
	}
	okCount, maxDur, total := 0, time.Duration(0), time.Duration(0)
	for i := 0; i < 20; i++ {
		r := <-results
		if r.ok {
			okCount++
		}
		total += r.dur
		maxDur = max(maxDur, r.dur)
	}
	t.Logf("  %d/20 correct, mean %.2fs, max %.2fs", okCount, (total / 20).Seconds(), maxDur.Seconds())
	if okCount != 20 {
		t.Error("random ranges mismatched")
	}

	t.Log("Client abort mid-stream:")
	actx, cancel := context.WithCancel(ctx)
	req, _ := http.NewRequestWithContext(actx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.CopyN(io.Discard, resp.Body, 1<<20)
	cancel()
	resp.Body.Close()
	select {
	case <-served:
		t.Log("  handler returned after client abort")
	case <-time.After(10 * time.Second):
		t.Error("  handler still running 10s after client abort")
	}

	fullRuns, _ := strconv.Atoi(os.Getenv("MT_LIVE_FULL"))
	for run := 1; run <= fullRuns; run++ {
		t.Logf("Full download %d/%d through the proxy:", run, fullRuns)
		before := proxy.Resumes()
		start := time.Now()
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		n, err := io.Copy(h, resp.Body)
		resp.Body.Close()
		<-served
		d := time.Since(start)
		sum := hex.EncodeToString(h.Sum(nil))
		t.Logf("  %d bytes in %.1fs (%.1f MB/s), upstream resumes %d, sha256 %s, err=%v", n, d.Seconds(), float64(n)/d.Seconds()/1e6, proxy.Resumes()-before, sum, err)
		if want := os.Getenv("MT_LIVE_SHA256"); want != "" && sum != want {
			t.Errorf("sha256 mismatch, want %s", want)
		}
		if n != size {
			t.Errorf("got %d bytes, want %d", n, size)
		}
	}
	if missing != 0 {
		t.Errorf("OnContentMissing fired %d times", missing)
	}
}
