package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type errList []error

func (l *errList) addf(format string, a ...any) { *l = append(*l, fmt.Errorf(format, a...)) }
func (l errList) err() error                    { return errors.Join(l...) }

// categoryRE matches what both backends accept as a category and what the
// sabdav adapter allows (ADR 0002).
var categoryRE = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

var keyIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// MinSigningKeyBytes is the minimum HMAC key length (spec §7.4).
const MinSigningKeyBytes = 32

// MinArgon2MemoryKiB is OWASP's lowest recommended argon2id memory cost.
const MinArgon2MemoryKiB = 19 * 1024

// Validate checks every value and returns all problems at once. Warnings are
// conditions that are allowed but worth telling the operator about.
func (c *Config) Validate() (warnings []string, err error) {
	var errs errList

	// server (spec §7.7)
	if w, err := checkListenAddr(c.Server.ListenAddr); err != nil {
		errs.addf("server.listenAddr: %v", err)
	} else if w != "" {
		warnings = append(warnings, "server.listenAddr: "+w)
	}
	if err := checkPublicURL(c.Server.PublicURL); err != nil {
		errs.addf("server.publicUrl: %v", err)
	}
	if len(c.Server.TrustedProxies) == 0 {
		errs.addf("server.trustedProxies: required; Mountenant only accepts requests from its reverse proxy")
	}
	for i, p := range c.Server.TrustedProxies {
		if _, err := ParsePrefix(p); err != nil {
			errs.addf("server.trustedProxies[%d]: %v", i, err)
		}
	}

	// database
	if c.Database.Path == "" {
		errs.addf("database.path: required")
	}
	if c.Database.BusyTimeoutMs <= 0 {
		errs.addf("database.busyTimeoutMs: must be > 0")
	}

	// session
	positive(&errs, "session.idleTimeout", c.Session.IdleTimeout)
	positive(&errs, "session.absoluteTimeout", c.Session.AbsoluteTimeout)
	if c.Session.AbsoluteTimeout.Duration < c.Session.IdleTimeout.Duration {
		errs.addf("session.absoluteTimeout: must be >= session.idleTimeout")
	}

	// signing (spec §7.4)
	if len(c.Signing.Keys) == 0 {
		errs.addf("signing.keys: at least one key is required (set MOUNTENANT_SIGNING_KEYS or MOUNTENANT_SIGNING_KEYS_FILE)")
	}
	ids := map[string]bool{}
	for i, k := range c.Signing.Keys {
		if !keyIDRE.MatchString(k.ID) {
			errs.addf("signing.keys[%d].id: %q must be 1-32 characters of A-Z, a-z, 0-9, '_', '-'", i, k.ID)
		} else if ids[k.ID] {
			errs.addf("signing.keys[%d].id: %q is used twice", i, k.ID)
		}
		ids[k.ID] = true
		if len(k.Secret) < MinSigningKeyBytes {
			errs.addf("signing.keys[%d].secret: must be at least %d bytes", i, MinSigningKeyBytes)
		}
	}
	positive(&errs, "signing.defaultTtl", c.Signing.DefaultTTL)
	positive(&errs, "signing.maxTtl", c.Signing.MaxTTL)
	if c.Signing.MaxTTL.Duration > 24*time.Hour {
		errs.addf("signing.maxTtl: must be <= 24h")
	}
	if c.Signing.DefaultTTL.Duration > c.Signing.MaxTTL.Duration {
		errs.addf("signing.defaultTtl: must be <= signing.maxTtl")
	}

	// backend (spec §10)
	switch c.Backend.Type {
	case "altmount", "nzbdav":
	case "":
		errs.addf("backend.type: required (altmount or nzbdav)")
	default:
		errs.addf("backend.type: %q is not supported (altmount or nzbdav)", c.Backend.Type)
	}
	if err := checkHTTPURL(c.Backend.APIURL); err != nil {
		errs.addf("backend.apiUrl: %v", err)
	}
	if err := checkHTTPURL(c.Backend.WebDAVURL); err != nil {
		errs.addf("backend.webdavUrl: %v", err)
	}
	if c.Backend.APIKey == "" {
		errs.addf("backend.apiKey: required")
	}
	if c.Backend.WebDAVUser == "" {
		errs.addf("backend.webdavUser: required")
	}
	if c.Backend.WebDAVPassword == "" {
		errs.addf("backend.webdavPassword: required")
	}
	if !categoryRE.MatchString(c.Backend.Category) {
		errs.addf("backend.category: %q must match %s", c.Backend.Category, categoryRE)
	}
	positive(&errs, "backend.requestTimeout", c.Backend.RequestTimeout)
	positive(&errs, "backend.pollInterval", c.Backend.PollInterval)
	positive(&errs, "backend.importTimeout", c.Backend.ImportTimeout)

	// jobs
	positive(&errs, "jobs.retention", c.Jobs.Retention)
	positive(&errs, "jobs.failedRetention", c.Jobs.FailedRetention)
	positive(&errs, "jobs.readyCheckInterval", c.Jobs.ReadyCheckInterval)
	positive(&errs, "jobs.orphanSweepInterval", c.Jobs.OrphanSweepInterval)
	if c.Jobs.MaxNZBBytesHardCap <= 0 {
		errs.addf("jobs.maxNzbBytesHardCap: must be > 0")
	}
	if c.Jobs.MaxFilesPerJob <= 0 {
		errs.addf("jobs.maxFilesPerJob: must be > 0")
	}

	// defaults.quota
	q := c.Defaults.Quota
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"maxActiveJobs", int64(q.MaxActiveJobs)},
		{"maxTotalJobs", int64(q.MaxTotalJobs)},
		{"maxNzbBytes", q.MaxNZBBytes},
		{"maxDownloadBytesPerDay", q.MaxDownloadBytesPerDay},
		{"maxConcurrentDownloads", int64(q.MaxConcurrentDownloads)},
	} {
		if f.v <= 0 {
			errs.addf("defaults.quota.%s: must be > 0", f.name)
		}
	}
	if q.MaxNZBBytes > c.Jobs.MaxNZBBytesHardCap && c.Jobs.MaxNZBBytesHardCap > 0 {
		warnings = append(warnings, fmt.Sprintf("defaults.quota.maxNzbBytes (%d) exceeds jobs.maxNzbBytesHardCap (%d); the hard cap applies", q.MaxNZBBytes, c.Jobs.MaxNZBBytesHardCap))
	}

	// rateLimits
	for _, rl := range []struct {
		name string
		RateLimit
	}{{"login", c.RateLimits.Login}, {"api", c.RateLimits.API}} {
		if rl.PerMinute <= 0 || rl.Burst <= 0 {
			errs.addf("rateLimits.%s: perMinute and burst must be > 0", rl.name)
		}
	}

	// logging
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		errs.addf("logging.level: %q must be debug, info, warn or error", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "json", "text":
	default:
		errs.addf("logging.format: %q must be json or text", c.Logging.Format)
	}
	if c.Logging.SecurityLog.Path == "" && !c.Logging.SecurityLog.Stdout {
		errs.addf("logging.securityLog: set path, stdout or both; fail2ban needs the security log")
	}

	// metrics
	if c.Metrics.Enabled {
		if _, err := checkListenAddr(c.Metrics.ListenAddr); err != nil {
			errs.addf("metrics.listenAddr: %v", err)
		} else if c.Metrics.ListenAddr == c.Server.ListenAddr {
			errs.addf("metrics.listenAddr: must differ from server.listenAddr, metrics are not public")
		}
	}

	// auth (spec §7.1)
	if c.Auth.MaxConcurrentHashes < 1 {
		errs.addf("auth.maxConcurrentHashes: must be >= 1")
	}
	positive(&errs, "auth.hashWaitTimeout", c.Auth.HashWaitTimeout)
	if c.Auth.Argon2.MemoryKiB < MinArgon2MemoryKiB {
		errs.addf("auth.argon2.memoryKiB: must be >= %d", MinArgon2MemoryKiB)
	}
	if c.Auth.Argon2.Iterations < 1 {
		errs.addf("auth.argon2.iterations: must be >= 1")
	}
	if c.Auth.Argon2.Parallelism < 1 {
		errs.addf("auth.argon2.parallelism: must be >= 1")
	}

	return warnings, errs.err()
}

func positive(errs *errList, name string, d Duration) {
	if d.Duration <= 0 {
		errs.addf("%s: must be a positive duration", name)
	}
}

// checkListenAddr requires a loopback or private address (spec §7.7). An
// unspecified address (":8080", "0.0.0.0:8080") is allowed with a warning,
// because inside a container it is the only way to be reachable from the
// proxy; the trusted-proxy check still rejects every other peer.
func checkListenAddr(addr string) (warning string, err error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%q: %v", addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("%q: port required", addr)
	}
	if host == "" {
		return "listening on all interfaces; make sure only the reverse proxy can reach it", nil
	}
	if host == "localhost" {
		return "", nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("%q: host must be an IP address", addr)
	}
	switch {
	case ip.IsUnspecified():
		return "listening on all interfaces; make sure only the reverse proxy can reach it", nil
	case ip.IsLoopback(), ip.IsPrivate():
		return "", nil
	}
	return "", fmt.Errorf("%q: must be a loopback or private address; Mountenant runs behind a reverse proxy", addr)
}

// checkPublicURL requires an https origin without path, query or fragment:
// the session cookie uses the __Host- prefix, which needs Path=/.
func checkPublicURL(s string) error {
	if s == "" {
		return errors.New("required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%q must use https; TLS is terminated by the reverse proxy", s)
	}
	if u.Host == "" || u.User != nil || strings.TrimSuffix(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must be an origin like https://dl.example.org (no path, query or credentials)", s)
	}
	return nil
}

func checkHTTPURL(s string) error {
	if s == "" {
		return errors.New("required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q must be an http(s) URL", s)
	}
	if u.User != nil {
		return errors.New("must not contain credentials; use the dedicated keys")
	}
	return nil
}

// ParsePrefix accepts a CIDR or a bare IP address (as a single-host prefix).
func ParsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return p.Masked(), nil
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is neither an IP address nor a CIDR", s)
	}
	return netip.PrefixFrom(ip, ip.BitLen()), nil
}
