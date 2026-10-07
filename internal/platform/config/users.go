package config

import (
	"fmt"
	"regexp"
	"strings"
)

// User is one entry of users.json (spec §8.3).
type User struct {
	Username     string         `json:"username"`
	PasswordHash string         `json:"passwordHash"`
	Quota        *QuotaOverride `json:"quota,omitempty"`
	Disabled     bool           `json:"disabled,omitempty"`
}

// QuotaOverride replaces individual fields of defaults.quota for one user.
type QuotaOverride struct {
	MaxActiveJobs          *int   `json:"maxActiveJobs,omitempty"`
	MaxTotalJobs           *int   `json:"maxTotalJobs,omitempty"`
	MaxNZBBytes            *int64 `json:"maxNzbBytes,omitempty"`
	MaxDownloadBytesPerDay *int64 `json:"maxDownloadBytesPerDay,omitempty"`
	MaxConcurrentDownloads *int   `json:"maxConcurrentDownloads,omitempty"`
}

// Apply returns base with the override's set fields replaced.
func (o *QuotaOverride) Apply(base Quota) Quota {
	if o == nil {
		return base
	}
	if o.MaxActiveJobs != nil {
		base.MaxActiveJobs = *o.MaxActiveJobs
	}
	if o.MaxTotalJobs != nil {
		base.MaxTotalJobs = *o.MaxTotalJobs
	}
	if o.MaxNZBBytes != nil {
		base.MaxNZBBytes = *o.MaxNZBBytes
	}
	if o.MaxDownloadBytesPerDay != nil {
		base.MaxDownloadBytesPerDay = *o.MaxDownloadBytesPerDay
	}
	if o.MaxConcurrentDownloads != nil {
		base.MaxConcurrentDownloads = *o.MaxConcurrentDownloads
	}
	return base
}

var usernameRE = regexp.MustCompile(`^[a-z0-9_.-]{3,32}$`)

// phcArgon2idRE matches the PHC string produced by `mountenant hash-password`:
// $argon2id$v=19$m=<kib>,t=<iter>,p=<par>$<b64 salt>$<b64 hash>.
var phcArgon2idRE = regexp.MustCompile(`^\$argon2id\$v=19\$m=\d+,t=\d+,p=\d+\$[A-Za-z0-9+/]+\$[A-Za-z0-9+/]+$`)

func validateUsers(users []User) error {
	var errs errList
	seen := map[string]int{}
	for i, u := range users {
		at := fmt.Sprintf("users[%d]", i)
		if !usernameRE.MatchString(u.Username) {
			errs.addf("%s.username: %q must be 3-32 characters of a-z, 0-9, '_', '.', '-'", at, u.Username)
		} else if j, dup := seen[strings.ToLower(u.Username)]; dup {
			errs.addf("%s.username: %q duplicates users[%d]", at, u.Username, j)
		} else {
			seen[strings.ToLower(u.Username)] = i
		}
		switch {
		case u.PasswordHash == "":
			errs.addf("%s.passwordHash: required", at)
		case !strings.HasPrefix(u.PasswordHash, "$"):
			// Never echo the value: it is probably a plaintext password.
			errs.addf("%s.passwordHash: must be an argon2id PHC hash from `mountenant hash-password`, plaintext passwords are rejected", at)
		case !phcArgon2idRE.MatchString(u.PasswordHash):
			errs.addf("%s.passwordHash: not an argon2id PHC string ($argon2id$v=19$m=..,t=..,p=..$salt$hash)", at)
		}
		if q := u.Quota; q != nil {
			for _, f := range []struct {
				name string
				v    int64
			}{
				{"maxActiveJobs", ptrVal(q.MaxActiveJobs)},
				{"maxTotalJobs", ptrVal(q.MaxTotalJobs)},
				{"maxNzbBytes", ptrVal(q.MaxNZBBytes)},
				{"maxDownloadBytesPerDay", ptrVal(q.MaxDownloadBytesPerDay)},
				{"maxConcurrentDownloads", ptrVal(q.MaxConcurrentDownloads)},
			} {
				if f.v <= 0 {
					errs.addf("%s.quota.%s: must be > 0", at, f.name)
				}
			}
		}
	}
	return errs.err()
}

// ptrVal returns 1 for an unset override so it passes the > 0 check.
func ptrVal[T int | int64](p *T) int64 {
	if p == nil {
		return 1
	}
	return int64(*p)
}
