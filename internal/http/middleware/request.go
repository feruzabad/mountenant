package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// RequestID returns the request's ID.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestIDs honours a well-formed X-Request-ID from the proxy or generates
// one, and echoes it in the response (spec §12.3).
func RequestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !requestIDRE.MatchString(id) {
			var b [12]byte
			_, _ = rand.Read(b[:])
			id = hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// SecurityHeaders sets the response headers of spec §7.7. Handlers may
// override individual headers (/dl sets Referrer-Policy: no-referrer).
func SecurityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
		"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; " +
		"form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// recorder captures status and size for the access log. Unwrap keeps
// http.ResponseController (flush, deadlines) working for streams.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// AccessLog logs one line per request. Download tokens are bearer secrets,
// so /dl/<token> is logged as /dl/REDACTED (spec §7.4).
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			defer func() {
				status := rec.status
				if status == 0 {
					status = http.StatusOK
				}
				log.LogAttrs(r.Context(), slog.LevelInfo, "request",
					slog.String("method", r.Method),
					slog.String("path", RedactPath(r.URL.Path)),
					slog.Int("status", status),
					slog.Int64("bytes", rec.bytes),
					slog.Duration("duration", time.Since(start)),
					slog.String("requestId", RequestID(r.Context())),
					slog.String("ip", ClientIP(r.Context()).String()),
				)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// RedactPath hides download tokens.
func RedactPath(p string) string {
	if strings.HasPrefix(p, "/dl/") {
		return "/dl/REDACTED"
	}
	return p
}

// Chain applies middleware so that the first one listed runs first.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
