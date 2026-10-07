// Package logging builds the application logger (log/slog) and the security
// event log consumed by fail2ban (spec §7.8, §12.3).
package logging

import (
	"fmt"
	"io"
	"log/slog"
)

// New returns the application logger for the given level ("debug", "info",
// "warn", "error") and format ("json" or "text").
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("logging level: %w", err)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("logging format %q: want json or text", format)
}
