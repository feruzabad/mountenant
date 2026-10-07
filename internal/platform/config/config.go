// Package config loads and validates Mountenant's configuration (spec §8):
// built-in defaults, then config.json, then users.json, then
// MOUNTENANT_<SECTION>_<KEY> environment variables, then *_FILE secrets.
// Unknown keys are errors and every value is validated before the process
// starts.
package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// Config is the content of config.json after all overrides.
type Config struct {
	// Schema lets editors find the JSON Schema; it is ignored otherwise.
	Schema     string     `json:"$schema,omitempty" env:"-"`
	Server     Server     `json:"server"`
	Database   Database   `json:"database"`
	Session    Session    `json:"session"`
	Signing    Signing    `json:"signing"`
	Backend    Backend    `json:"backend"`
	Jobs       Jobs       `json:"jobs"`
	Defaults   Defaults   `json:"defaults"`
	RateLimits RateLimits `json:"rateLimits"`
	Logging    Logging    `json:"logging"`
	Metrics    Metrics    `json:"metrics"`
	Auth       Auth       `json:"auth"`
}

type Server struct {
	ListenAddr     string   `json:"listenAddr"`
	PublicURL      string   `json:"publicUrl"`
	TrustedProxies []string `json:"trustedProxies"`
}

type Database struct {
	Path          string `json:"path"`
	BusyTimeoutMs int    `json:"busyTimeoutMs"`
}

type Session struct {
	IdleTimeout     Duration `json:"idleTimeout"`
	AbsoluteTimeout Duration `json:"absoluteTimeout"`
}

type Signing struct {
	Keys       []SigningKey `json:"keys"`
	DefaultTTL Duration     `json:"defaultTtl"`
	MaxTTL     Duration     `json:"maxTtl"`
}

// SigningKey is one HMAC key for download links (spec §7.4). The first key
// signs; all keys verify.
type SigningKey struct {
	ID     string `json:"id"`
	Secret string `json:"secret" secret:"true"`
}

type Backend struct {
	Type           string   `json:"type"`
	APIURL         string   `json:"apiUrl"`
	APIKey         string   `json:"apiKey" secret:"true"`
	WebDAVURL      string   `json:"webdavUrl"`
	WebDAVUser     string   `json:"webdavUser"`
	WebDAVPassword string   `json:"webdavPassword" secret:"true"`
	Category       string   `json:"category"`
	RequestTimeout Duration `json:"requestTimeout"`
	PollInterval   Duration `json:"pollInterval"`
	ImportTimeout  Duration `json:"importTimeout"`
}

type Jobs struct {
	Retention           Duration `json:"retention"`
	MaxNZBBytesHardCap  int64    `json:"maxNzbBytesHardCap"`
	FailedRetention     Duration `json:"failedRetention"`
	ReadyCheckInterval  Duration `json:"readyCheckInterval"`
	OrphanSweepInterval Duration `json:"orphanSweepInterval"`
	OrphanSweepDryRun   bool     `json:"orphanSweepDryRun"`
	MaxFilesPerJob      int      `json:"maxFilesPerJob"`
}

type Defaults struct {
	Quota Quota `json:"quota"`
}

// Quota holds the per-user limits (spec §4.1).
type Quota struct {
	MaxActiveJobs          int   `json:"maxActiveJobs"`
	MaxTotalJobs           int   `json:"maxTotalJobs"`
	MaxNZBBytes            int64 `json:"maxNzbBytes"`
	MaxDownloadBytesPerDay int64 `json:"maxDownloadBytesPerDay"`
	MaxConcurrentDownloads int   `json:"maxConcurrentDownloads"`
}

type RateLimits struct {
	Login RateLimit `json:"login"`
	API   RateLimit `json:"api"`
}

type RateLimit struct {
	PerMinute int `json:"perMinute"`
	Burst     int `json:"burst"`
}

type Logging struct {
	Level       string      `json:"level"`
	Format      string      `json:"format"`
	SecurityLog SecurityLog `json:"securityLog"`
}

type SecurityLog struct {
	Path   string `json:"path"`
	Stdout bool   `json:"stdout"`
}

type Metrics struct {
	Enabled    bool   `json:"enabled"`
	ListenAddr string `json:"listenAddr"`
}

type Auth struct {
	MaxConcurrentHashes int      `json:"maxConcurrentHashes"`
	HashWaitTimeout     Duration `json:"hashWaitTimeout"`
	Argon2              Argon2   `json:"argon2"`
}

// Argon2 holds the argon2id cost parameters (RFC 9106).
type Argon2 struct {
	MemoryKiB   uint32 `json:"memoryKiB" env:"MEMORY_KIB"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
}

const (
	KiB = 1 << 10
	MiB = 1 << 20
	GiB = 1 << 30
)

// Default returns the built-in defaults (precedence level 1).
func Default() Config {
	return Config{
		Server: Server{ListenAddr: "127.0.0.1:8080"},
		Database: Database{
			Path:          "/var/lib/mountenant/mountenant.db",
			BusyTimeoutMs: 5000,
		},
		Session: Session{
			IdleTimeout:     Duration{12 * time.Hour},
			AbsoluteTimeout: Duration{7 * 24 * time.Hour},
		},
		Signing: Signing{
			DefaultTTL: Duration{15 * time.Minute},
			MaxTTL:     Duration{24 * time.Hour},
		},
		Backend: Backend{
			Category:       "mountenant",
			RequestTimeout: Duration{30 * time.Second},
			PollInterval:   Duration{10 * time.Second},
			ImportTimeout:  Duration{30 * time.Minute},
		},
		Jobs: Jobs{
			Retention:           Duration{168 * time.Hour},
			MaxNZBBytesHardCap:  64 * MiB,
			FailedRetention:     Duration{24 * time.Hour},
			ReadyCheckInterval:  Duration{time.Hour},
			OrphanSweepInterval: Duration{6 * time.Hour},
			MaxFilesPerJob:      10000,
		},
		Defaults: Defaults{Quota: Quota{
			MaxActiveJobs:          5,
			MaxTotalJobs:           50,
			MaxNZBBytes:            32 * MiB,
			MaxDownloadBytesPerDay: 100 * GiB,
			MaxConcurrentDownloads: 4,
		}},
		RateLimits: RateLimits{
			Login: RateLimit{PerMinute: 10, Burst: 5},
			API:   RateLimit{PerMinute: 300, Burst: 60},
		},
		Logging: Logging{
			Level:       "info",
			Format:      "json",
			SecurityLog: SecurityLog{Path: "/var/log/mountenant/security.log"},
		},
		Metrics: Metrics{ListenAddr: "127.0.0.1:9090"},
		Auth: Auth{
			MaxConcurrentHashes: 2,
			HashWaitTimeout:     Duration{2 * time.Second},
			Argon2:              Argon2{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 4},
		},
	}
}

// Duration is a time.Duration written as a Go duration string ("15m", "168h").
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"15m\"")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

const redacted = "REDACTED"

// Redacted returns a copy with every secret replaced, safe to log
// (spec §8.1: the effective config is logged at startup).
func (c Config) Redacted() Config {
	c.Signing.Keys = append([]SigningKey(nil), c.Signing.Keys...)
	redact(reflect.ValueOf(&c).Elem())
	return c
}

func redact(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if t.Field(i).Tag.Get("secret") == "true" && f.Kind() == reflect.String && f.String() != "" {
				f.SetString(redacted)
				continue
			}
			redact(f)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			redact(v.Index(i))
		}
	}
}
