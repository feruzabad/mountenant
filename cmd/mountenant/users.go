package main

import (
	"fmt"

	"github.com/feruzabad/mountenant/internal/identity/domain"
	"github.com/feruzabad/mountenant/internal/platform/config"
)

// configuredUsers maps users.json entries to the domain, merging each user's
// quota override into defaults.quota.
func configuredUsers(l *config.Loaded) ([]domain.ConfiguredUser, error) {
	out := make([]domain.ConfiguredUser, 0, len(l.Users))
	for i, u := range l.Users {
		name, err := domain.ParseUsername(u.Username)
		if err != nil {
			return nil, fmt.Errorf("users[%d]: %w", i, err)
		}
		q := u.Quota.Apply(l.Config.Defaults.Quota)
		out = append(out, domain.ConfiguredUser{
			Username:     name,
			PasswordHash: u.PasswordHash,
			Disabled:     u.Disabled,
			Quota: domain.Quota{
				MaxActiveJobs:          q.MaxActiveJobs,
				MaxTotalJobs:           q.MaxTotalJobs,
				MaxNZBBytes:            q.MaxNZBBytes,
				MaxDownloadBytesPerDay: q.MaxDownloadBytesPerDay,
				MaxConcurrentDownloads: q.MaxConcurrentDownloads,
			},
		})
	}
	return out, nil
}
