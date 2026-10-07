package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/platform/clock"
)

func TestReadiness(t *testing.T) {
	var backendDown atomic.Bool
	backendDown.Store(true)
	var calls atomic.Int32
	c := clock.NewFake(time.Unix(0, 0))
	rd := &Readiness{
		Now: c.Now,
		Checks: []Check{
			{"database", func(context.Context) error { return nil }},
			{"backend", func(context.Context) error {
				calls.Add(1)
				if backendDown.Load() {
					return errors.New("dial tcp 10.0.0.5:8080: connection refused")
				}
				return nil
			}},
		},
	}
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		rd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec
	}

	rec := get()
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), `"backend":"fail"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Fatal("error details leaked")
	}

	backendDown.Store(false)
	get()
	if calls.Load() != 1 {
		t.Fatalf("result not cached: %d calls", calls.Load())
	}
	c.Advance(6 * time.Second)
	if rec := get(); rec.Code != 200 || calls.Load() != 2 {
		t.Fatalf("%d after cache expiry, %d calls", rec.Code, calls.Load())
	}
}

func TestReadinessTimeout(t *testing.T) {
	rd := &Readiness{
		Timeout: 20 * time.Millisecond,
		Checks: []Check{{"slow", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}},
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	rd.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != 503 || time.Since(start) > time.Second {
		t.Fatalf("%d after %v", rec.Code, time.Since(start))
	}
}

func TestLiveness(t *testing.T) {
	rec := httptest.NewRecorder()
	Liveness(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(rec.Code)
	}
}
