package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/feruzabad/mountenant/internal/http/middleware"
	"github.com/feruzabad/mountenant/internal/http/problem"
	identityapp "github.com/feruzabad/mountenant/internal/identity/app"
	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/logging"
)

// SessionCookie is the session cookie name. The __Host- prefix makes
// browsers require Secure, Path=/ and no Domain (spec §7.2).
const SessionCookie = "__Host-mountenant_session"

// CSRFHeader carries the synchronizer token on unsafe requests.
const CSRFHeader = "X-CSRF-Token"

// maxJSONBody bounds JSON request bodies before decoding.
const maxJSONBody = 64 << 10

type ctxKey int

const (
	principalKey ctxKey = iota
	cookieKey
)

type principal struct {
	user    domain.User
	session domain.Session
}

func principalFrom(ctx context.Context) principal {
	p, _ := ctx.Value(principalKey).(principal)
	return p
}

func cookieToken(ctx context.Context) string {
	t, _ := ctx.Value(cookieKey).(string)
	return t
}

func sessionCookie(token string, maxAge time.Duration) string {
	return (&http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(maxAge / time.Second),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}).String()
}

func isPublic(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/login"
}

func isSafe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// guard bounds the body, checks Origin and the CSRF token on unsafe
// requests (spec §7.3) and authenticates the session cookie for every route
// except login.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		ctx := r.Context()
		if c, err := r.Cookie(SessionCookie); err == nil {
			ctx = context.WithValue(ctx, cookieKey, c.Value)
		}
		unsafe := !isSafe(r.Method)

		if unsafe && !s.sameOrigin(r) {
			s.csrfFailure(w, r, "", "origin")
			return
		}
		if isPublic(r) {
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		user, sess, err := s.Auth.Authenticate(ctx, cookieToken(ctx))
		switch {
		case errors.Is(err, identityapp.ErrUnauthenticated):
			problem.Write(w, http.StatusUnauthorized, problem.Unauthenticated, "Not logged in", "")
			return
		case err != nil:
			s.writeError(w, r, err)
			return
		}
		if unsafe && !sess.CheckCSRF(r.Header.Get(CSRFHeader)) {
			s.csrfFailure(w, r, string(user.Username), "token")
			return
		}
		ctx = context.WithValue(ctx, principalKey, principal{user: user, session: sess})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sameOrigin requires an Origin equal to the public URL and, when the
// browser sends Fetch Metadata, Sec-Fetch-Site: same-origin.
func (s *Server) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	return strings.ToLower(r.Header.Get("Origin")) == s.PublicOrigin
}

func (s *Server) csrfFailure(w http.ResponseWriter, r *http.Request, user, reason string) {
	_ = s.Security.Log(logging.SecurityEvent{
		Name: logging.EventCSRFFailure, IP: middleware.ClientIP(r.Context()), User: user, Reason: reason,
	})
	problem.Write(w, http.StatusForbidden, problem.CSRFFailed, "CSRF check failed", "")
}
