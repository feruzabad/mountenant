package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/feruzabad/mountenant/internal/platform/logging"
)

type secEvents []logging.SecurityEvent

func (s *secEvents) Log(e logging.SecurityEvent) error { *s = append(*s, e); return nil }

func TestTrustedProxies(t *testing.T) {
	var events secEvents
	tp := &TrustedProxies{
		Prefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("127.0.0.1/32")},
		Exempt:   map[string]bool{"/healthz": true},
		Security: &events,
	}
	var got netip.Addr
	h := tp.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ClientIP(r.Context()) }))

	cases := []struct {
		name, peer, path string
		xff              []string
		status           int
		client           string
	}{
		{"no xff", "10.0.0.1:5000", "/", nil, 200, "10.0.0.1"},
		{"one hop", "10.0.0.1:5000", "/", []string{"203.0.113.9"}, 200, "203.0.113.9"},
		{"spoofed left part ignored", "10.0.0.1:5000", "/", []string{"1.1.1.1, 203.0.113.9"}, 200, "203.0.113.9"},
		{"proxy chain", "10.0.0.1:5000", "/", []string{"203.0.113.9, 10.0.0.2"}, 200, "203.0.113.9"},
		{"split headers", "10.0.0.1:5000", "/", []string{"6.6.6.6", "203.0.113.9"}, 200, "203.0.113.9"},
		{"garbage from client", "10.0.0.1:5000", "/", []string{"<script>, 203.0.113.9"}, 200, "203.0.113.9"},
		{"garbage only", "10.0.0.1:5000", "/", []string{"nonsense"}, 200, "10.0.0.1"},
		{"mapped v6", "[::ffff:10.0.0.1]:5000", "/", []string{"2001:db8::1"}, 200, "2001:db8::1"},
		{"untrusted peer", "203.0.113.50:5000", "/", []string{"10.0.0.1"}, 403, ""},
		{"untrusted peer, exempt probe", "172.17.0.1:5000", "/healthz", nil, 200, "172.17.0.1"},
	}
	for _, c := range cases {
		got = netip.Addr{}
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		r.RemoteAddr = c.peer
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.status {
			t.Errorf("%s: status %d", c.name, rec.Code)
			continue
		}
		if c.status == 200 && got.String() != c.client {
			t.Errorf("%s: client %s, want %s", c.name, got, c.client)
		}
	}
	if len(events) != 1 || events[0].Name != logging.EventForbiddenPeer || events[0].IP.String() != "203.0.113.50" {
		t.Fatalf("events %+v", events)
	}
}

func TestRequestIDsAndHeaders(t *testing.T) {
	var seen string
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = RequestID(r.Context()) }), RequestIDs, SecurityHeaders)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if seen != "abc-123" || rec.Header().Get("X-Request-ID") != "abc-123" {
		t.Fatalf("honoured id: %q", seen)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", rec.Header())
	}

	r.Header.Set("X-Request-ID", "bad id\nwith newline")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if len(seen) != 24 || strings.Contains(seen, " ") {
		t.Fatalf("malformed id not replaced: %q", seen)
	}
}

func TestAccessLogRedactsTokens(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h := AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("hello"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/dl/eyJ2IjoxfQ.c2lnbmF0dXJl", nil))
	out := buf.String()
	if strings.Contains(out, "eyJ2") || !strings.Contains(out, `"path":"/dl/REDACTED"`) {
		t.Fatalf("token not redacted: %s", out)
	}
	if !strings.Contains(out, `"status":206`) || !strings.Contains(out, `"bytes":5`) {
		t.Fatalf("status/bytes: %s", out)
	}
}

func TestRecorderKeepsFlusher(t *testing.T) {
	h := AccessLog(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through recorder: %v", err)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}
