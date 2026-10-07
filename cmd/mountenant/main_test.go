package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/identity/adapters/argon2id"
)

const (
	testAPIKey   = "api-key-must-not-be-logged"
	testDavPass  = "dav-password-must-not-be-logged"
	testSignKey  = "signing-key-must-not-be-logged-0123456789"
	testPassword = "correct horse battery"
)

type result struct {
	code           int
	stdout, stderr string
}

func runCmd(t *testing.T, environ []string, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), env{args: args, environ: environ, stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb})
	return result{code, out.String(), errb.String()}
}

// setup writes config.json and users.json into a temp dir and returns the
// environment that points at them.
func setup(t *testing.T, backendURL string) (environ []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	hash, err := argon2id.Hasher{Params: argon2id.Params{MemoryKiB: 19456, Iterations: 1, Parallelism: 1}}.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"server": map[string]any{
			"listenAddr":     "127.0.0.1:0",
			"publicUrl":      "https://dl.example.org",
			"trustedProxies": []string{"127.0.0.1"},
		},
		"database": map[string]any{"path": filepath.Join(dir, "data", "mountenant.db")},
		"backend": map[string]any{
			"type":       "altmount",
			"apiUrl":     backendURL,
			"webdavUrl":  backendURL,
			"webdavUser": "dav",
		},
		"logging": map[string]any{"securityLog": map[string]any{"path": filepath.Join(dir, "security.log")}},
	}
	users := []map[string]any{{"username": "alice", "passwordHash": hash}}
	for name, v := range map[string]any{"config.json": cfg, "users.json": users} {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keys := filepath.Join(dir, "keys.json")
	os.WriteFile(keys, []byte(`[{"id":"k1","secret":"`+testSignKey+`"}]`), 0o600)
	return []string{
		"MOUNTENANT_CONFIG=" + filepath.Join(dir, "config.json"),
		"MOUNTENANT_BACKEND_API_KEY=" + testAPIKey,
		"MOUNTENANT_BACKEND_WEBDAV_PASSWORD=" + testDavPass,
		"MOUNTENANT_SIGNING_KEYS_FILE=" + keys,
	}, dir
}

func TestUsageAndVersion(t *testing.T) {
	if r := runCmd(t, nil, ""); r.code != 2 || !strings.Contains(r.stderr, "Usage") {
		t.Errorf("no args: %+v", r)
	}
	if r := runCmd(t, nil, "", "frobnicate"); r.code != 2 {
		t.Errorf("unknown: %+v", r)
	}
	if r := runCmd(t, nil, "", "config", "check"); r.code != 2 {
		t.Errorf("config check: %+v", r)
	}
	if r := runCmd(t, nil, "", "version"); r.code != 0 || r.stdout == "" {
		t.Errorf("version: %+v", r)
	}
}

func TestHashPassword(t *testing.T) {
	fast := []string{"-m", "19456", "-t", "1", "-p", "1"}
	r := runCmd(t, nil, testPassword+"\n", append([]string{"hash-password"}, fast...)...)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	enc := strings.TrimSpace(r.stdout)
	if ok, err := (argon2id.Hasher{}).Verify(testPassword, enc); !ok || err != nil {
		t.Fatalf("hash %q does not verify: %v", enc, err)
	}
	if !strings.HasPrefix(enc, "$argon2id$v=19$m=19456,t=1,p=1$") {
		t.Fatalf("parameters not applied: %s", enc)
	}
	if r := runCmd(t, nil, "short\n", append([]string{"hash-password"}, fast...)...); r.code != 1 || !strings.Contains(r.stderr, "at least 12") {
		t.Errorf("short password: %+v", r)
	}
	if r := runCmd(t, nil, testPassword, "hash-password", "-m", "1024"); r.code != 1 {
		t.Errorf("weak parameters accepted: %+v", r)
	}
}

func TestConfigValidate(t *testing.T) {
	environ, _ := setup(t, "http://backend:8080")
	r := runCmd(t, environ, "", "config", "validate")
	if r.code != 0 || !strings.Contains(r.stdout, "configuration OK") || !strings.Contains(r.stdout, "1 users") {
		t.Fatalf("%+v", r)
	}
	r = runCmd(t, append(environ, "MOUNTENANT_BACKEND_TYPE=sabnzbd"), "", "config", "validate")
	if r.code != 1 || !strings.Contains(r.stderr, "backend.type") {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(r.stderr, testAPIKey) {
		t.Fatal("secret in error output")
	}
}

func TestMigrate(t *testing.T) {
	environ, _ := setup(t, "http://backend:8080")
	for i := 0; i < 2; i++ {
		r := runCmd(t, environ, "", "migrate")
		if r.code != 0 || !strings.Contains(r.stdout, "schema version 1") {
			t.Fatalf("run %d: %+v", i, r)
		}
	}
}

// fakeBackend answers the AltMount readiness probe.
func fakeBackend(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sabnzbd/api" && r.URL.Query().Get("apikey") == testAPIKey:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"version":"0.3.2"}`))
		case r.Method == "PROPFIND" && r.URL.Path == "/webdav/complete/mountenant/":
			if u, p, ok := r.BasicAuth(); !ok || u != "dav" || p != testDavPass {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNotFound) // namespace not created yet
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServe(t *testing.T) {
	backend := fakeBackend(t)
	environ, dir := setup(t, backend.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan string, 1)
	var out, errb syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, env{
			args: []string{"serve"}, environ: environ, stdin: strings.NewReader(""),
			stdout: &out, stderr: &errb,
			listening: func(a string) { addrc <- a },
		})
	}()

	var addr string
	select {
	case addr = <-addrc:
	case code := <-done:
		t.Fatalf("serve exited with %d: %s %s", code, out.String(), errb.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}

	for path, want := range map[string]int{"/healthz": 200, "/readyz": 200, "/nope": 404} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}

	// SIGHUP re-reads users.json (UC-04).
	hash, _ := argon2id.Hasher{Params: argon2id.Params{MemoryKiB: 19456, Iterations: 1, Parallelism: 1}}.Hash(testPassword)
	users, _ := json.Marshal([]map[string]any{{"username": "alice", "passwordHash": hash}, {"username": "bob", "passwordHash": hash}})
	if err := os.WriteFile(filepath.Join(dir, "users.json"), users, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), `"configured":2,"created":1`) {
		if time.Now().After(deadline) {
			t.Fatalf("users not re-synced after SIGHUP:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}

	logs := out.String() + errb.String()
	for _, s := range []string{testAPIKey, testDavPass, testSignKey} {
		if strings.Contains(logs, s) {
			t.Errorf("secret %q in logs", s)
		}
	}
	for _, s := range []string{`"msg":"starting"`, `"msg":"listening"`, `"msg":"stopped"`, `"REDACTED"`, `"msg":"users synced","configured":1,"created":1`} {
		if !strings.Contains(logs, s) {
			t.Errorf("logs lack %s:\n%s", s, logs)
		}
	}
	for _, p := range []string{"security.log", "data/mountenant.db"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Error(err)
		}
	}
}

func TestServeRejectsInvalidConfig(t *testing.T) {
	environ, _ := setup(t, "http://backend:8080")
	r := runCmd(t, append(environ, "MOUNTENANT_SERVER_PUBLIC_URL=http://insecure.example.org"), "", "serve")
	if r.code != 1 || !strings.Contains(r.stderr, "server.publicUrl") {
		t.Fatalf("%+v", r)
	}
}
