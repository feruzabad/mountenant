package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testHash = "$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"

var secret32 = strings.Repeat("s", 32)

// minimal is the smallest valid config.json.
func minimal() map[string]any {
	return map[string]any{
		"server": map[string]any{
			"publicUrl":      "https://dl.example.org",
			"trustedProxies": []string{"127.0.0.1"},
		},
		"signing": map[string]any{"keys": []map[string]string{{"id": "k1", "secret": secret32}}},
		"backend": map[string]any{
			"type":           "altmount",
			"apiUrl":         "http://backend:8080/sabnzbd/api",
			"apiKey":         "key",
			"webdavUrl":      "http://backend:8080/webdav",
			"webdavUser":     "dav",
			"webdavPassword": "pw",
		},
	}
}

type fixture struct {
	dir string
	env []string
}

func newFixture(t *testing.T, cfg any, users any) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{dir: dir, env: []string{"MOUNTENANT_CONFIG=" + filepath.Join(dir, "config.json"), "HOME=/ignored"}}
	write := func(name string, v any) {
		if v == nil {
			return
		}
		b, ok := v.([]byte)
		if !ok {
			var err error
			if b, err = json.Marshal(v); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("config.json", cfg)
	write("users.json", users)
	return f
}

func (f *fixture) load(extraEnv ...string) (*Loaded, error) {
	return Load(append(append([]string{}, f.env...), extraEnv...))
}

func TestLoadDefaultsAndFiles(t *testing.T) {
	f := newFixture(t, minimal(), []map[string]any{{"username": "alice", "passwordHash": testHash}})
	l, err := f.load()
	if err != nil {
		t.Fatal(err)
	}
	c := l.Config
	if c.Backend.Category != "mountenant" || c.Backend.ImportTimeout.Duration != 30*time.Minute {
		t.Errorf("backend defaults not applied: %+v", c.Backend)
	}
	if c.Jobs.Retention.Duration != 168*time.Hour || c.Jobs.MaxFilesPerJob != 10000 {
		t.Errorf("jobs defaults not applied: %+v", c.Jobs)
	}
	if c.Server.PublicURL != "https://dl.example.org" {
		t.Errorf("file value not applied: %q", c.Server.PublicURL)
	}
	if len(l.Users) != 1 || l.Users[0].Username != "alice" {
		t.Errorf("users: %+v", l.Users)
	}
	if l.UsersPath != filepath.Join(f.dir, "users.json") {
		t.Errorf("users path %q", l.UsersPath)
	}
}

func TestEnvOverridesAndSecretFiles(t *testing.T) {
	f := newFixture(t, minimal(), []any{})
	keyFile := filepath.Join(f.dir, "keys.json")
	os.WriteFile(keyFile, []byte(`[{"id":"new","secret":"`+strings.Repeat("n", 40)+`"},{"id":"old","secret":"`+secret32+`"}]`+"\n"), 0o600)
	apiKeyFile := filepath.Join(f.dir, "apikey")
	os.WriteFile(apiKeyFile, []byte("from-file\n"), 0o600)

	l, err := f.load(
		"MOUNTENANT_SERVER_PUBLIC_URL=https://other.example.org",
		"MOUNTENANT_SERVER_TRUSTED_PROXIES=10.0.0.0/8, 172.16.0.1",
		"MOUNTENANT_BACKEND_TYPE=nzbdav",
		"MOUNTENANT_BACKEND_API_KEY_FILE="+apiKeyFile,
		"MOUNTENANT_SIGNING_KEYS_FILE="+keyFile,
		"MOUNTENANT_JOBS_RETENTION=48h",
		"MOUNTENANT_JOBS_ORPHAN_SWEEP_DRY_RUN=true",
		"MOUNTENANT_DEFAULTS_QUOTA_MAX_NZB_BYTES=1000",
		"MOUNTENANT_LOGGING_SECURITY_LOG_STDOUT=1",
		"MOUNTENANT_AUTH_ARGON2_MEMORY_KIB=32768",
		"OTHER_VAR=ignored",
	)
	if err != nil {
		t.Fatal(err)
	}
	c := l.Config
	switch {
	case c.Server.PublicURL != "https://other.example.org":
		t.Error("publicUrl")
	case len(c.Server.TrustedProxies) != 2 || c.Server.TrustedProxies[1] != "172.16.0.1":
		t.Errorf("trustedProxies %q", c.Server.TrustedProxies)
	case c.Backend.Type != "nzbdav" || c.Backend.APIKey != "from-file":
		t.Errorf("backend %q %q", c.Backend.Type, c.Backend.APIKey)
	case len(c.Signing.Keys) != 2 || c.Signing.Keys[0].ID != "new":
		t.Errorf("signing keys %+v", c.Signing.Keys)
	case c.Jobs.Retention.Duration != 48*time.Hour || !c.Jobs.OrphanSweepDryRun:
		t.Errorf("jobs %+v", c.Jobs)
	case c.Defaults.Quota.MaxNZBBytes != 1000:
		t.Error("quota")
	case !c.Logging.SecurityLog.Stdout || c.Auth.Argon2.MemoryKiB != 32768:
		t.Error("nested")
	}
}

func TestExplicitMissingUsersFileFails(t *testing.T) {
	l, err := Load([]string{
		"MOUNTENANT_USERS=" + filepath.Join(t.TempDir(), "missing.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "users file") {
		t.Fatalf("explicit missing users file must fail, got %v (%v)", err, l)
	}
}

func TestRejectsUnknownAndInvalid(t *testing.T) {
	cfg := minimal()
	cfg["server"].(map[string]any)["publicUrl"] = "http://dl.example.org/app"
	cfg["backend"].(map[string]any)["category"] = "bad/cat"
	f := newFixture(t, cfg, nil)
	_, err := f.load("MOUNTENANT_BACKEND_API_KYE=x", "MOUNTENANT_JOBS_RETENTION=soon")
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"MOUNTENANT_BACKEND_API_KYE: unknown setting", "MOUNTENANT_JOBS_RETENTION", "server.publicUrl", "backend.category"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}

	cfg = minimal()
	cfg["server"].(map[string]any)["listenAdress"] = "x"
	if _, err := newFixture(t, cfg, nil).load(); err == nil || !strings.Contains(err.Error(), `unknown field "listenAdress"`) {
		t.Errorf("unknown JSON key: %v", err)
	}
}

func TestValidationRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"public listen", func(c *Config) { c.Server.ListenAddr = "8.8.8.8:80" }, "server.listenAddr"},
		{"no proxies", func(c *Config) { c.Server.TrustedProxies = nil }, "server.trustedProxies"},
		{"bad proxy", func(c *Config) { c.Server.TrustedProxies = []string{"proxy"} }, "server.trustedProxies[0]"},
		{"short key", func(c *Config) { c.Signing.Keys[0].Secret = "short" }, "signing.keys[0].secret"},
		{"dup key", func(c *Config) { c.Signing.Keys = append(c.Signing.Keys, c.Signing.Keys[0]) }, "used twice"},
		{"ttl", func(c *Config) { c.Signing.DefaultTTL.Duration = 48 * time.Hour }, "signing.defaultTtl"},
		{"max ttl", func(c *Config) { c.Signing.MaxTTL.Duration = 48 * time.Hour }, "signing.maxTtl"},
		{"type", func(c *Config) { c.Backend.Type = "sabnzbd" }, "backend.type"},
		{"api url", func(c *Config) { c.Backend.APIURL = "backend:8080" }, "backend.apiUrl"},
		{"url creds", func(c *Config) { c.Backend.WebDAVURL = "http://u:p@backend" }, "credentials"},
		{"session", func(c *Config) { c.Session.AbsoluteTimeout.Duration = time.Hour }, "session.absoluteTimeout"},
		{"quota", func(c *Config) { c.Defaults.Quota.MaxActiveJobs = 0 }, "defaults.quota.maxActiveJobs"},
		{"argon2", func(c *Config) { c.Auth.Argon2.MemoryKiB = 1024 }, "auth.argon2.memoryKiB"},
		{"metrics", func(c *Config) { c.Metrics.Enabled = true; c.Metrics.ListenAddr = c.Server.ListenAddr }, "metrics.listenAddr"},
		{"security log", func(c *Config) { c.Logging.SecurityLog = SecurityLog{} }, "logging.securityLog"},
		{"level", func(c *Config) { c.Logging.Level = "trace" }, "logging.level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			_, err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
		})
	}

	c := validConfig()
	c.Server.ListenAddr = ":8080"
	c.Defaults.Quota.MaxNZBBytes = c.Jobs.MaxNZBBytesHardCap + 1
	w, err := c.Validate()
	if err != nil || len(w) != 2 {
		t.Fatalf("want 2 warnings and no error, got %q, %v", w, err)
	}
}

func validConfig() Config {
	c := Default()
	c.Server.PublicURL = "https://dl.example.org"
	c.Server.TrustedProxies = []string{"127.0.0.1"}
	c.Signing.Keys = []SigningKey{{ID: "k1", Secret: secret32}}
	c.Backend.Type = "altmount"
	c.Backend.APIURL = "http://backend:8080/sabnzbd/api"
	c.Backend.APIKey = "key"
	c.Backend.WebDAVURL = "http://backend:8080/webdav"
	c.Backend.WebDAVUser = "dav"
	c.Backend.WebDAVPassword = "pw"
	return c
}

func TestUsersValidation(t *testing.T) {
	zero := 0
	users := []User{
		{Username: "alice", PasswordHash: testHash},
		{Username: "Alice", PasswordHash: testHash},
		{Username: "bob", PasswordHash: "hunter2"},
		{Username: "carol", PasswordHash: "$argon2i$v=19$m=1,t=1,p=1$a$b"},
		{Username: "dave", PasswordHash: testHash, Quota: &QuotaOverride{MaxActiveJobs: &zero}},
		{Username: "eve", PasswordHash: testHash},
		{Username: "alice", PasswordHash: testHash},
	}
	err := validateUsers(users)
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"users[1].username", "users[2].passwordHash: must be an argon2id", "users[3].passwordHash: not an argon2id", "users[4].quota.maxActiveJobs", "users[6].username: \"alice\" duplicates users[0]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "hunter2") {
		t.Error("plaintext password echoed in error")
	}
	if strings.Contains(msg, "users[5]") {
		t.Error("valid user reported")
	}
}

func TestQuotaOverride(t *testing.T) {
	n := int64(7)
	got := (&QuotaOverride{MaxNZBBytes: &n}).Apply(Default().Defaults.Quota)
	want := Default().Defaults.Quota
	want.MaxNZBBytes = 7
	if got != want {
		t.Fatalf("got %+v", got)
	}
}

func TestRedacted(t *testing.T) {
	c := validConfig()
	r := c.Redacted()
	b, _ := json.Marshal(r)
	for _, s := range []string{secret32, `"key"`, `"pw"`} {
		if strings.Contains(string(b), s) {
			t.Errorf("redacted config contains %s", s)
		}
	}
	if c.Signing.Keys[0].Secret != secret32 || c.Backend.APIKey != "key" {
		t.Error("Redacted modified the original")
	}
}

func TestEnvSecretFileConflicts(t *testing.T) {
	f := newFixture(t, minimal(), []any{})
	p := filepath.Join(f.dir, "k")
	os.WriteFile(p, []byte("x"), 0o600)
	_, err := f.load("MOUNTENANT_BACKEND_API_KEY=a", "MOUNTENANT_BACKEND_API_KEY_FILE="+p)
	if err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("got %v", err)
	}
}

func TestWorldReadableUsersWarning(t *testing.T) {
	f := newFixture(t, minimal(), []any{})
	os.Chmod(filepath.Join(f.dir, "users.json"), 0o644)
	l, err := f.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "world-readable") {
		t.Fatalf("warnings %q", l.Warnings)
	}
}
