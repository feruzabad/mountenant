// Package handlers implements the generated API interface (api/openapi.yaml).
// Handlers are thin: decode, call one app handler, encode (spec §13.2).
package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/feruzabad/mountenant/internal/http/api"
	"github.com/feruzabad/mountenant/internal/http/middleware"
	"github.com/feruzabad/mountenant/internal/http/problem"
	identityapp "github.com/feruzabad/mountenant/internal/identity/app"
	"github.com/feruzabad/mountenant/internal/identity/domain"
)

// Usage is what UC-03 reports next to the quota.
type Usage struct {
	ActiveJobs           int
	TotalJobs            int
	DownloadedBytesToday int64
	ActiveDownloads      int
}

// UsageReader supplies a user's current usage.
type UsageReader interface {
	Usage(ctx context.Context, user domain.UserID) (Usage, error)
}

// Server implements api.StrictServerInterface.
type Server struct {
	Auth   *identityapp.Auth
	Usage  UsageReader
	Policy domain.SessionPolicy
	// PublicOrigin is the configured public URL without trailing slash; the
	// Origin of unsafe requests must equal it (spec §7.3).
	PublicOrigin string
	Security     identityapp.SecurityLog
	Logger       *slog.Logger
}

var _ api.StrictServerInterface = (*Server)(nil)

// Handler returns the /api/ handler: session and CSRF guard, then the
// generated router.
func (s *Server) Handler() http.Handler {
	s.PublicOrigin = strings.ToLower(strings.TrimSuffix(s.PublicOrigin, "/"))
	strict := api.NewStrictHandlerWithOptions(s, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			problem.Write(w, http.StatusBadRequest, problem.BadRequest, "Bad request", "The request body is not valid JSON for this operation.")
		},
		ResponseErrorHandlerFunc: s.writeError,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, http.StatusNotFound, problem.NotFound, "Not found", "")
	})
	h := api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			problem.Write(w, http.StatusBadRequest, problem.BadRequest, "Bad request", "")
		},
	})
	return s.guard(h)
}

// badRequest is a validation failure found by a handler.
type badRequest string

func (b badRequest) Error() string { return string(b) }

// writeError maps errors returned by handlers to Problem Details, in one
// place (spec §13.2).
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var rl *identityapp.RateLimitedError
	var br badRequest
	switch {
	case errors.As(err, &br):
		problem.Write(w, http.StatusBadRequest, problem.BadRequest, "Bad request", string(br))
	case errors.Is(err, identityapp.ErrInvalidCredentials):
		problem.Write(w, http.StatusUnauthorized, problem.Unauthenticated, "Invalid username or password", "")
	case errors.Is(err, identityapp.ErrUnauthenticated):
		problem.Write(w, http.StatusUnauthorized, problem.Unauthenticated, "Not logged in", "")
	case errors.As(err, &rl):
		problem.WriteRateLimited(w, rl.RetryAfter)
	case errors.Is(err, context.Canceled):
		// The client went away; nobody reads the answer.
	default:
		s.Logger.ErrorContext(r.Context(), "request failed", "err", err, "requestId", middleware.RequestID(r.Context()))
		problem.Write(w, http.StatusInternalServerError, problem.Internal, "Internal error", "")
	}
}

// Login implements UC-01.
func (s *Server) Login(ctx context.Context, req api.LoginRequestObject) (api.LoginResponseObject, error) {
	b := req.Body
	if b == nil || b.Username == "" || b.Password == "" || len(b.Username) > 64 || len(b.Password) > 1024 {
		return nil, badRequest("username and password are required (username at most 64 bytes, password at most 1024)")
	}
	res, err := s.Auth.Login(ctx, identityapp.LoginInput{
		Username:      b.Username,
		Password:      b.Password,
		IP:            middleware.ClientIP(ctx),
		PreviousToken: cookieToken(ctx),
	})
	if err != nil {
		return nil, err
	}
	c := sessionCookie(res.Token, s.Policy.AbsoluteTimeout)
	return api.Login204Response{Headers: api.Login204ResponseHeaders{SetCookie: &c}}, nil
}

// Logout implements UC-02.
func (s *Server) Logout(ctx context.Context, _ api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	if err := s.Auth.Logout(ctx, cookieToken(ctx)); err != nil {
		return nil, err
	}
	c := sessionCookie("", -time.Second)
	return api.Logout204Response{Headers: api.Logout204ResponseHeaders{SetCookie: &c}}, nil
}

// GetCsrfToken returns the synchronizer token of the session.
func (s *Server) GetCsrfToken(ctx context.Context, _ api.GetCsrfTokenRequestObject) (api.GetCsrfTokenResponseObject, error) {
	p := principalFrom(ctx)
	return api.GetCsrfToken200JSONResponse{Token: p.session.CSRFToken()}, nil
}

// GetMe implements UC-03.
func (s *Server) GetMe(ctx context.Context, _ api.GetMeRequestObject) (api.GetMeResponseObject, error) {
	p := principalFrom(ctx)
	u, err := s.Usage.Usage(ctx, p.user.ID)
	if err != nil {
		return nil, err
	}
	q := p.user.Quota
	return api.GetMe200JSONResponse{
		Username: string(p.user.Username),
		Quota: api.Quota{
			MaxActiveJobs:          q.MaxActiveJobs,
			MaxTotalJobs:           q.MaxTotalJobs,
			MaxNzbBytes:            q.MaxNZBBytes,
			MaxDownloadBytesPerDay: q.MaxDownloadBytesPerDay,
			MaxConcurrentDownloads: q.MaxConcurrentDownloads,
		},
		Usage: api.Usage{
			ActiveJobs:           u.ActiveJobs,
			TotalJobs:            u.TotalJobs,
			DownloadedBytesToday: u.DownloadedBytesToday,
			ActiveDownloads:      u.ActiveDownloads,
		},
	}, nil
}
