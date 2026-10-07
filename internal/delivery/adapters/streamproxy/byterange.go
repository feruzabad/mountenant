package streamproxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

// rangeDecision is the outcome of evaluating a request's Range headers
// against the catalogued file.
type rangeDecision struct {
	rng           *domain.ByteRange // nil: serve the whole file with 200
	unsatisfiable bool              // answer 416
}

// resolveRange implements RFC 9110 §14 for one file of known size, using
// Mountenant's own validators. Only the result is ever sent to a backend:
// a closed range inside the file, or nothing (ADR 0003).
//
//   - No Range, a non-bytes unit or invalid syntax: whole file.
//   - If-Range that does not match the strong ETag or the exact
//     Last-Modified: whole file.
//   - More than one range: whole file (permitted by RFC 9110 §14.2).
//   - "a-b" / "a-": clamped to the file end; a >= size is unsatisfiable.
//   - "-n": the last n bytes (all of them if n >= size); n = 0 is unsatisfiable.
func resolveRange(h http.Header, size int64, etag string, lastModified time.Time) rangeDecision {
	spec := h.Get("Range")
	if spec == "" || size <= 0 {
		return rangeDecision{}
	}
	if ir := h.Get("If-Range"); ir != "" && !ifRangeMatches(ir, etag, lastModified) {
		return rangeDecision{}
	}
	unit, set, ok := strings.Cut(spec, "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return rangeDecision{}
	}
	parts := strings.Split(set, ",")
	if len(parts) != 1 {
		return rangeDecision{}
	}
	first, last, ok := strings.Cut(strings.TrimSpace(parts[0]), "-")
	if !ok {
		return rangeDecision{}
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)

	if first == "" { // suffix range
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return rangeDecision{}
		}
		if n == 0 {
			return rangeDecision{unsatisfiable: true}
		}
		n = min(n, size)
		return rangeDecision{rng: &domain.ByteRange{Start: size - n, End: size - 1}}
	}

	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return rangeDecision{}
	}
	end := size - 1
	if last != "" {
		e, err := strconv.ParseInt(last, 10, 64)
		if err != nil || e < start {
			return rangeDecision{}
		}
		end = min(e, size-1)
	}
	if start >= size {
		return rangeDecision{unsatisfiable: true}
	}
	return rangeDecision{rng: &domain.ByteRange{Start: start, End: end}}
}

// ifRangeMatches applies RFC 9110 §13.1.5: an entity tag must match strongly;
// a date must equal Last-Modified exactly.
func ifRangeMatches(v, etag string, lastModified time.Time) bool {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "W/") {
		return !strings.HasPrefix(v, "W/") && etag != "" && v == etag
	}
	t, err := http.ParseTime(v)
	return err == nil && !lastModified.IsZero() && t.Equal(lastModified.UTC().Truncate(time.Second))
}
