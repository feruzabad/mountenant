package problem

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, 404, NotFound, "Not found", "")
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if rec.Header().Get("Content-Type") != "application/problem+json" || p.Code != NotFound || p.Status != 404 || p.Type != "urn:mountenant:problem:not_found" {
		t.Fatalf("%+v %v", p, rec.Header())
	}
}

func TestWriteRateLimited(t *testing.T) {
	for d, want := range map[time.Duration]string{1500 * time.Millisecond: "2", 0: "1", 10 * time.Second: "10"} {
		rec := httptest.NewRecorder()
		WriteRateLimited(rec, d)
		if rec.Code != 429 || rec.Header().Get("Retry-After") != want {
			t.Errorf("%v: %d %q", d, rec.Code, rec.Header().Get("Retry-After"))
		}
	}
}
