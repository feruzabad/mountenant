package streamproxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/jobs/domain"
)

func TestResolveRange(t *testing.T) {
	const size = 1000
	const etag = `"abc"`
	lm := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r := func(a, b int64) *domain.ByteRange { return &domain.ByteRange{Start: a, End: b} }

	tests := []struct {
		name    string
		hdr     map[string]string
		want    *domain.ByteRange
		unsatis bool
	}{
		{"no range", nil, nil, false},
		{"closed", map[string]string{"Range": "bytes=0-9"}, r(0, 9), false},
		{"open ended", map[string]string{"Range": "bytes=990-"}, r(990, 999), false},
		{"end past EOF is clamped", map[string]string{"Range": "bytes=990-5000"}, r(990, 999), false},
		{"suffix", map[string]string{"Range": "bytes=-10"}, r(990, 999), false},
		{"suffix larger than file", map[string]string{"Range": "bytes=-5000"}, r(0, 999), false},
		{"suffix zero", map[string]string{"Range": "bytes=-0"}, nil, true},
		{"start at EOF", map[string]string{"Range": "bytes=1000-1010"}, nil, true},
		{"multi range serves whole file", map[string]string{"Range": "bytes=0-9,20-29"}, nil, false},
		{"other unit ignored", map[string]string{"Range": "items=0-9"}, nil, false},
		{"garbage ignored", map[string]string{"Range": "bytes=x-y"}, nil, false},
		{"reversed ignored", map[string]string{"Range": "bytes=9-0"}, nil, false},
		{"spaces tolerated", map[string]string{"Range": "bytes= 5 - 9 "}, r(5, 9), false},
		{"if-range etag match", map[string]string{"Range": "bytes=0-9", "If-Range": etag}, r(0, 9), false},
		{"if-range etag mismatch", map[string]string{"Range": "bytes=0-9", "If-Range": `"zzz"`}, nil, false},
		{"if-range weak etag never matches", map[string]string{"Range": "bytes=0-9", "If-Range": `W/"abc"`}, nil, false},
		{"if-range date match", map[string]string{"Range": "bytes=0-9", "If-Range": lm.Format(http.TimeFormat)}, r(0, 9), false},
		{"if-range date mismatch", map[string]string{"Range": "bytes=0-9", "If-Range": lm.Add(-time.Hour).Format(http.TimeFormat)}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.hdr {
				h.Set(k, v)
			}
			got := resolveRange(h, size, etag, lm)
			if got.unsatisfiable != tt.unsatis {
				t.Fatalf("unsatisfiable = %v, want %v", got.unsatisfiable, tt.unsatis)
			}
			switch {
			case tt.want == nil && got.rng != nil:
				t.Fatalf("range = %+v, want whole file", *got.rng)
			case tt.want != nil && (got.rng == nil || *got.rng != *tt.want):
				t.Fatalf("range = %v, want %+v", got.rng, *tt.want)
			}
			if got.rng != nil && (got.rng.Start < 0 || got.rng.End >= size || got.rng.Start > got.rng.End) {
				t.Fatalf("range %+v escapes the file", *got.rng)
			}
		})
	}
}
