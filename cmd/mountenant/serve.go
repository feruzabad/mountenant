package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/feruzabad/mountenant/internal/http/health"
	"github.com/feruzabad/mountenant/internal/http/middleware"
	identitysqlite "github.com/feruzabad/mountenant/internal/identity/adapters/sqlite"
	identityapp "github.com/feruzabad/mountenant/internal/identity/app"
	"github.com/feruzabad/mountenant/internal/jobs/adapters/sabdav"
	"github.com/feruzabad/mountenant/internal/platform/clock"
	"github.com/feruzabad/mountenant/internal/platform/config"
	"github.com/feruzabad/mountenant/internal/platform/db"
	"github.com/feruzabad/mountenant/internal/platform/id"
	"github.com/feruzabad/mountenant/internal/platform/logging"
)

// shutdownGrace is how long in-flight requests may take after SIGTERM
// (spec §12.2).
const shutdownGrace = 30 * time.Second

func migrateCmd(ctx context.Context, e env) error {
	l, err := loadConfig(e)
	if err != nil {
		return err
	}
	log, err := logging.New(e.stderr, l.Config.Logging.Level, l.Config.Logging.Format)
	if err != nil {
		return err
	}
	d, err := db.Open(ctx, l.Config.Database.Path, l.Config.Database.BusyTimeoutMs)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := db.Migrate(ctx, d, log); err != nil {
		return err
	}
	v, err := db.Version(ctx, d)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.stdout, "database %s at schema version %d\n", l.Config.Database.Path, v)
	return nil
}

func serve(ctx context.Context, e env) error {
	l, err := loadConfig(e)
	if err != nil {
		return err
	}
	cfg := l.Config
	log, err := logging.New(e.stdout, cfg.Logging.Level, cfg.Logging.Format)
	if err != nil {
		return err
	}
	for _, w := range l.Warnings {
		log.Warn("configuration warning", "warning", w)
	}
	log.Info("starting", "version", buildVersion(), "configFile", l.ConfigPath, "usersFile", l.UsersPath, "config", cfg.Redacted())

	var secStdout = e.stdout
	if !cfg.Logging.SecurityLog.Stdout {
		secStdout = nil
	}
	seclog, err := logging.OpenSecurityLog(cfg.Logging.SecurityLog.Path, secStdout, nil)
	if err != nil {
		return err
	}
	defer seclog.Close()

	database, err := db.Open(ctx, cfg.Database.Path, cfg.Database.BusyTimeoutMs)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := db.Migrate(ctx, database, log); err != nil {
		return err
	}

	clk := clock.System{}
	store := identitysqlite.New(database)
	userSync := &identityapp.UserSync{Users: store, Clock: clk, IDs: &id.UUIDv7{Clock: clk}}
	syncUsers := func(l *config.Loaded) error {
		users, err := configuredUsers(l)
		if err != nil {
			return err
		}
		res, err := userSync.Sync(ctx, users)
		if err != nil {
			return err
		}
		log.Info("users synced", "configured", len(users), "created", res.Created, "updated", res.Updated, "sessionsRevoked", res.Revoked)
		return nil
	}
	if err := syncUsers(l); err != nil {
		return err
	}

	backend, err := newBackend(cfg.Backend)
	if err != nil {
		return err
	}

	ready := &health.Readiness{
		Logger: log,
		Checks: []health.Check{
			{Name: "database", Fn: database.CheckWritable},
			{Name: "backend", Fn: backend.Ping},
		},
		Timeout: min(cfg.Backend.RequestTimeout.Duration, 5*time.Second),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Liveness)
	mux.Handle("GET /readyz", ready)

	var prefixes []netip.Prefix
	for _, p := range cfg.Server.TrustedProxies {
		pfx, err := config.ParsePrefix(p)
		if err != nil {
			return err
		}
		prefixes = append(prefixes, pfx)
	}
	proxies := &middleware.TrustedProxies{
		Prefixes: prefixes,
		Exempt:   map[string]bool{"/healthz": true, "/readyz": true},
		Security: seclog,
	}
	handler := middleware.Chain(mux,
		middleware.RequestIDs,
		middleware.SecurityHeaders,
		proxies.Wrap,
		middleware.AccessLog(log),
	)

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Server.ListenAddr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String(), "publicUrl", cfg.Server.PublicURL, "backend", cfg.Backend.Type)
	if e.listening != nil {
		e.listening(ln.Addr().String())
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	for {
		select {
		case <-hup:
			// UC-04: users.json is re-read on SIGHUP. Other settings need a
			// restart. An invalid file keeps the current users.
			if nl, err := loadConfig(e); err != nil {
				log.Error("reloading users failed; keeping current users", "err", err)
			} else if err := syncUsers(nl); err != nil {
				log.Error("user sync failed", "err", err)
			}
			if err := seclog.Reopen(); err != nil {
				log.Error("reopening security log", "err", err)
			} else {
				log.Info("security log reopened")
			}
		case err := <-served:
			return err
		case <-ctx.Done():
			log.Info("shutting down", "grace", shutdownGrace)
			sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			err := srv.Shutdown(sctx)
			if errors.Is(err, context.DeadlineExceeded) {
				err = srv.Close()
			}
			if serr := <-served; !errors.Is(serr, http.ErrServerClosed) {
				err = errors.Join(err, serr)
			}
			log.Info("stopped")
			return err
		}
	}
}

// newBackend builds the sabdav adapter for the configured product. The
// transport bounds waiting for response headers by requestTimeout; bodies of
// downloads are not bounded, they are cancelled through their context.
func newBackend(c config.Backend) (*sabdav.Adapter, error) {
	profile, ok := sabdav.ProfileByName(c.Type)
	if !ok {
		return nil, fmt.Errorf("backend.type %q has no profile", c.Type)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = c.RequestTimeout.Duration
	return sabdav.New(sabdav.Config{
		APIURL:      c.APIURL,
		APIKey:      c.APIKey,
		DavURL:      c.WebDAVURL,
		DavUser:     c.WebDAVUser,
		DavPassword: c.WebDAVPassword,
		Category:    c.Category,
	}, profile, &http.Client{Transport: tr})
}
