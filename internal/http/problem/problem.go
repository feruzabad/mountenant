// Package problem writes RFC 9457 Problem Details responses with the stable
// error codes of spec §6.5.
package problem

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Problem is an application/problem+json body. Code is Mountenant's
// machine-readable error code; Type is derived from it.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// TypeURI is the stable type of an error code.
func TypeURI(code string) string { return "urn:mountenant:problem:" + code }

// Error codes (spec §6.5).
const (
	InvalidNZB         = "invalid_nzb"
	Unauthenticated    = "unauthenticated"
	CSRFFailed         = "csrf_failed"
	LinkInvalid        = "link_invalid"
	Forbidden          = "forbidden"
	NotFound           = "not_found"
	JobNotReady        = "job_not_ready"
	NZBTooLarge        = "nzb_too_large"
	QuotaExceeded      = "quota_exceeded"
	RateLimited        = "rate_limited"
	BackendUnavailable = "backend_unavailable"
	DownloadLimit      = "download_limit"
	ContentMissing     = "content_missing"
	BadRequest         = "bad_request"
	Internal           = "internal"
)

// Write sends a problem response.
func Write(w http.ResponseWriter, status int, code, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Type: TypeURI(code), Title: title, Status: status, Code: code, Detail: detail})
}

// WriteRateLimited sends 429 with Retry-After in whole seconds, rounded up.
func WriteRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	secs := int((retryAfter + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
	Write(w, http.StatusTooManyRequests, RateLimited, "Too many requests", "")
}
