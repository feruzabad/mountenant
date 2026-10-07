package main

import (
	"errors"
	"fmt"

	"github.com/feruzabad/mountenant/internal/identity/adapters/argon2id"
	"github.com/feruzabad/mountenant/internal/platform/config"
)

// loadConfig loads and validates the configuration, including a full decode
// of every password hash (the loader only checks their shape).
func loadConfig(e env) (*config.Loaded, error) {
	l, err := config.Load(e.environ)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	var errs []error
	for i, u := range l.Users {
		if _, _, _, err := argon2id.Decode(u.PasswordHash); err != nil {
			errs = append(errs, fmt.Errorf("users[%d].passwordHash: %w", i, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return l, nil
}

func validateConfig(e env) error {
	l, err := loadConfig(e)
	if err != nil {
		return err
	}
	for _, w := range l.Warnings {
		fmt.Fprintln(e.stderr, "warning:", w)
	}
	src := l.ConfigPath
	if src == "" {
		src = "environment only"
	}
	fmt.Fprintf(e.stdout, "configuration OK (%s; %d users; backend %s)\n", src, len(l.Users), l.Config.Backend.Type)
	return nil
}
