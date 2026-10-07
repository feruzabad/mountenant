// Package health serves /healthz (liveness) and /readyz (readiness, spec
// §12.3).
package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Check reports whether one dependency is usable.
type Check struct {
	Name string
	Fn   func(context.Context) error
}

// Liveness answers 200 while the process can serve requests.
func Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readiness runs its checks concurrently and answers 200 when all pass, 503
// otherwise. The endpoint is unauthenticated, so the body names failing
// checks but never their errors (those are logged), and results are cached
// for CacheFor so probes cannot be used to load the backend.
type Readiness struct {
	Checks   []Check
	Timeout  time.Duration // per check; 0 means 3s
	CacheFor time.Duration // 0 means 5s
	Logger   *slog.Logger
	Now      func() time.Time

	mu      sync.Mutex
	at      time.Time
	results map[string]string
	ok      bool
}

func (rd *Readiness) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	results, ok := rd.evaluate(r.Context())
	body := map[string]any{"status": "ok", "checks": results}
	code := http.StatusOK
	if !ok {
		body["status"] = "unavailable"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, body)
}

func (rd *Readiness) evaluate(ctx context.Context) (map[string]string, bool) {
	now := time.Now
	if rd.Now != nil {
		now = rd.Now
	}
	cacheFor := rd.CacheFor
	if cacheFor == 0 {
		cacheFor = 5 * time.Second
	}
	// Holding the lock while checking also collapses concurrent probes into
	// one round of checks.
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.results != nil && now().Sub(rd.at) < cacheFor {
		return rd.results, rd.ok
	}

	timeout := rd.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	// Probes must not be cancelled by an impatient client: the result is
	// shared through the cache.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	errs := make([]error, len(rd.Checks))
	var wg sync.WaitGroup
	for i, c := range rd.Checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Fn(ctx)
		}()
	}
	wg.Wait()

	results := make(map[string]string, len(rd.Checks))
	ok := true
	for i, c := range rd.Checks {
		results[c.Name] = "ok"
		if errs[i] != nil {
			results[c.Name] = "fail"
			ok = false
			if rd.Logger != nil {
				rd.Logger.Warn("readiness check failed", "check", c.Name, "err", errs[i])
			}
		}
	}
	rd.at, rd.results, rd.ok = now(), results, ok
	return results, ok
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
