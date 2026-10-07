package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/feruzabad/mountenant/internal/http/health"
	"github.com/feruzabad/mountenant/internal/jobs/adapters/sabdav"
	"github.com/feruzabad/mountenant/internal/platform/config"
	"github.com/feruzabad/mountenant/internal/platform/db"
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

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
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

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	for {
		select {
		case <-hup:
			// UC-04 (users sync) hooks in here with the identity slice.
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
