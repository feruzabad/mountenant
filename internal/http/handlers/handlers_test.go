package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/http/middleware"
	"github.com/feruzabad/mountenant/internal/identity/adapters/argon2id"
	identitysqlite "github.com/feruzabad/mountenant/internal/identity/adapters/sqlite"
	identityapp "github.com/feruzabad/mountenant/internal/identity/app"
	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/db"
	"github.com/feruzabad/mountenant/internal/platform/logging"
	"github.com/feruzabad/mountenant/internal/platform/ratelimit"
)

const origin = "https://dl.example.org"

type secLog struct{ events []logging.SecurityEvent }

func (s *secLog) Log(e logging.SecurityEvent) error { s.events = append(s.events, e); return nil }

type fixedUsage struct{}

func (fixedUsage) Usage(context.Context, domain.UserID) (Usage, error) {
	return Usage{ActiveJobs: 1, TotalJobs: 2, DownloadedBytesToday: 3, ActiveDownloads: 4}, nil
}

type env struct {
	h   http.Handler
	sec *secLog
}

func newEnv(t *testing.T, loginPerMinute int) *env {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "t.db"), 5000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := db.Migrate(ctx, d, nil); err != nil {
		t.Fatal(err)
	}
	store := identitysqlite.New(d)
	hasher := argon2id.Hasher{Params: argon2id.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1}}
	hash, _ := hasher.Hash("correct horse battery")
	q := domain.Quota{MaxActiveJobs: 5, MaxTotalJobs: 50, MaxNZBBytes: 1 << 20, MaxDownloadBytesPerDay: 1 << 30, MaxConcurrentDownloads: 2}
	if err := store.ApplySync(ctx, domain.SyncPlan{Create: []domain.User{{ID: "u1", Username: "alice", PasswordHash: hash, Quota: q, ConfigVersion: "v", CreatedAt: time.Now(), UpdatedAt: time.Now()}}}); err != nil {
		t.Fatal(err)
	}
	sec := &secLog{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	policy := domain.SessionPolicy{IdleTimeout: time.Hour, AbsoluteTimeout: 24 * time.Hour}
	auth, err := identityapp.NewAuth(identityapp.Auth{
		Users: store, Sessions: store, Hasher: hasher, Clock: clock.System{}, Policy: policy,
		Security: sec, Logger: log,
		IPLimit:     &ratelimit.Buckets{PerMinute: loginPerMinute, Burst: loginPerMinute},
		UserLimit:   &ratelimit.Buckets{PerMinute: loginPerMinute, Burst: loginPerMinute},
		UserBackoff: &ratelimit.Backoff{Free: 5, Base: time.Second, Max: time.Minute},
	}, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Auth: auth, Usage: fixedUsage{}, Policy: policy, PublicOrigin: origin + "/", Security: sec, Logger: log}
	return &env{h: s.Handler(), sec: sec}
}

type req struct {
	method, path, body, cookie, csrf string
	origin                           *string
	fetchSite                        string
}

func (e *env) do(r req) *httptest.ResponseRecorder {
	hr := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	hr = hr.WithContext(middleware.WithClientIP(hr.Context(), netip.MustParseAddr("203.0.113.9")))
	if r.body != "" {
		hr.Header.Set("Content-Type", "application/json")
	}
	o := origin
	if r.origin != nil {
		o = *r.origin
	}
	if o != "" {
		hr.Header.Set("Origin", o)
	}
	if r.fetchSite != "" {
		hr.Header.Set("Sec-Fetch-Site", r.fetchSite)
	}
	if r.cookie != "" {
		hr.AddCookie(&http.Cookie{Name: SessionCookie, Value: r.cookie})
	}
	if r.csrf != "" {
		hr.Header.Set(CSRFHeader, r.csrf)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, hr)
	return rec
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content type %q, body %s", ct, rec.Body)
	}
	var p struct{ Code string }
	json.Unmarshal(rec.Body.Bytes(), &p)
	return p.Code
}

const goodLogin = `{"username":"alice","password":"correct horse battery"}`

func (e *env) login(t *testing.T) string {
	t.Helper()
	rec := e.do(req{method: "POST", path: "/api/v1/auth/login", body: goodLogin})
	if rec.Code != 204 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie {
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func TestLoginMeLogout(t *testing.T) {
	e := newEnv(t, 100)

	rec := e.do(req{method: "POST", path: "/api/v1/auth/login", body: goodLogin})
	if rec.Code != 204 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	sc := rec.Header().Get("Set-Cookie")
	for _, want := range []string{SessionCookie + "=", "Path=/", "Max-Age=86400", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(sc, want) {
			t.Errorf("cookie %q lacks %s", sc, want)
		}
	}
	if strings.Contains(sc, "Domain") {
		t.Error("__Host- cookie must not set Domain")
	}
	token := rec.Result().Cookies()[0].Value

	rec = e.do(req{method: "GET", path: "/api/v1/me", cookie: token})
	var me struct {
		Username string
		Quota    struct{ MaxActiveJobs int }
		Usage    struct{ ActiveDownloads int }
	}
	json.Unmarshal(rec.Body.Bytes(), &me)
	if rec.Code != 200 || me.Username != "alice" || me.Quota.MaxActiveJobs != 5 || me.Usage.ActiveDownloads != 4 {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("API responses must not be cached")
	}
	if rec := e.do(req{method: "GET", path: "/api/v1/nope", cookie: token}); rec.Code != 404 || code(t, rec) != "not_found" {
		t.Fatalf("unknown route: %d", rec.Code)
	}

	// Logout is unsafe: it needs the CSRF token.
	if rec := e.do(req{method: "POST", path: "/api/v1/auth/logout", cookie: token}); rec.Code != 403 || code(t, rec) != "csrf_failed" {
		t.Fatalf("logout without token: %d", rec.Code)
	}
	rec = e.do(req{method: "GET", path: "/api/v1/csrf", cookie: token})
	var csrf struct{ Token string }
	json.Unmarshal(rec.Body.Bytes(), &csrf)
	if rec.Code != 200 || csrf.Token == "" {
		t.Fatalf("csrf: %d %s", rec.Code, rec.Body)
	}
	rec = e.do(req{method: "POST", path: "/api/v1/auth/logout", cookie: token, csrf: csrf.Token})
	if rec.Code != 204 || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout: %d %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
	if rec := e.do(req{method: "GET", path: "/api/v1/me", cookie: token}); rec.Code != 401 || code(t, rec) != "unauthenticated" {
		t.Fatalf("me after logout: %d", rec.Code)
	}
}

func TestLoginRotatesSession(t *testing.T) {
	e := newEnv(t, 100)
	first := e.login(t)
	rec := e.do(req{method: "POST", path: "/api/v1/auth/login", body: goodLogin, cookie: first})
	if rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if rec := e.do(req{method: "GET", path: "/api/v1/me", cookie: first}); rec.Code != 401 {
		t.Fatalf("old session still valid: %d", rec.Code)
	}
}

func TestLoginErrors(t *testing.T) {
	e := newEnv(t, 2) // two logins per IP pass, the third is throttled
	empty := ""
	cross := "https://evil.example"
	cases := []struct {
		name   string
		r      req
		status int
		code   string
	}{
		{"wrong password", req{method: "POST", path: "/api/v1/auth/login", body: `{"username":"alice","password":"nope"}`}, 401, "unauthenticated"},
		{"unknown user", req{method: "POST", path: "/api/v1/auth/login", body: `{"username":"mallory","password":"nope"}`}, 401, "unauthenticated"},
		{"missing field", req{method: "POST", path: "/api/v1/auth/login", body: `{"username":"alice"}`}, 400, "bad_request"},
		{"bad json", req{method: "POST", path: "/api/v1/auth/login", body: `{`}, 400, "bad_request"},
		{"no origin", req{method: "POST", path: "/api/v1/auth/login", body: goodLogin, origin: &empty}, 403, "csrf_failed"},
		{"foreign origin", req{method: "POST", path: "/api/v1/auth/login", body: goodLogin, origin: &cross}, 403, "csrf_failed"},
		{"cross-site fetch", req{method: "POST", path: "/api/v1/auth/login", body: goodLogin, fetchSite: "cross-site"}, 403, "csrf_failed"},
		{"rate limited", req{method: "POST", path: "/api/v1/auth/login", body: goodLogin}, 429, "rate_limited"},
		// Anonymous callers learn nothing about which routes exist.
		{"unknown route, anonymous", req{method: "GET", path: "/api/v1/nope"}, 401, "unauthenticated"},
	}
	for _, c := range cases {
		rec := e.do(c.r)
		if rec.Code != c.status {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
			continue
		}
		if got := code(t, rec); got != c.code {
			t.Errorf("%s: code %q", c.name, got)
		}
		if c.status == 429 && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", c.name)
		}
	}
	names := map[string]int{}
	for _, ev := range e.sec.events {
		names[ev.Name]++
	}
	if names[logging.EventAuthFailure] != 2 || names[logging.EventCSRFFailure] != 3 || names[logging.EventAuthRateLimited] != 1 {
		t.Fatalf("security events %v", names)
	}
}

func TestBodyLimit(t *testing.T) {
	e := newEnv(t, 100)
	big := `{"username":"alice","password":"` + strings.Repeat("x", maxJSONBody) + `"}`
	if rec := e.do(req{method: "POST", path: "/api/v1/auth/login", body: big}); rec.Code != 400 {
		t.Fatalf("oversized body: %d", rec.Code)
	}
}
