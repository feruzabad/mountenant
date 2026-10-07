// Package middleware holds the HTTP middleware shared by all routes.
package middleware

import (
	"context"
	"net/http"
	"net/netip"
	"strings"

	"github.com/feruzabad/mountenant/internal/http/problem"
	"github.com/feruzabad/mountenant/internal/platform/logging"
)

type ctxKey int

const (
	clientIPKey ctxKey = iota
	requestIDKey
)

// ClientIP returns the client address resolved by TrustedProxies.
func ClientIP(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(clientIPKey).(netip.Addr)
	return ip
}

// WithClientIP stores ip in ctx (tests and internal callers).
func WithClientIP(ctx context.Context, ip netip.Addr) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

// TrustedProxies enforces that every request arrives through the reverse
// proxy (spec §7.7) and resolves the real client IP: the rightmost address
// in X-Forwarded-For that is not a trusted proxy. Requests from any other
// peer get 403 and a forbidden_peer security event.
//
// Paths in Exempt (exact match) skip the peer check, so container and
// orchestrator probes can reach /healthz and /readyz directly; they carry
// no data.
type TrustedProxies struct {
	Prefixes []netip.Prefix
	Exempt   map[string]bool
	Security interface {
		Log(logging.SecurityEvent) error
	}
}

func (t *TrustedProxies) trusted(ip netip.Addr) bool {
	for _, p := range t.Prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (t *TrustedProxies) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := peerAddr(r)
		if t.Exempt[r.URL.Path] {
			next.ServeHTTP(w, r.WithContext(WithClientIP(r.Context(), peer)))
			return
		}
		if !peer.IsValid() || !t.trusted(peer) {
			if t.Security != nil {
				_ = t.Security.Log(logging.SecurityEvent{Name: logging.EventForbiddenPeer, IP: peer, Reason: "untrusted_peer"})
			}
			problem.Write(w, http.StatusForbidden, problem.Forbidden, "Forbidden", "")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithClientIP(r.Context(), t.clientIP(r, peer))))
	})
}

func peerAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// clientIP walks X-Forwarded-For from the right, skipping trusted proxies.
// Entries left of the first untrusted address are client-controlled and
// ignored.
func (t *TrustedProxies) clientIP(r *http.Request, peer netip.Addr) netip.Addr {
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// Trusted proxies append valid addresses, so this entry came
			// from the client; the last good hop is the best answer.
			break
		}
		client = ip.Unmap()
		if !t.trusted(client) {
			break
		}
	}
	return client
}
