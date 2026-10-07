package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// UserID is a UUIDv7 string, immutable.
type UserID string

// Username is 3-32 characters of [a-z0-9_.-]. Usernames are unique
// case-insensitively; the canonical form is lowercase.
type Username string

var usernameRE = regexp.MustCompile(`^[a-z0-9_.-]{3,32}$`)

// ErrInvalidUsername is returned by ParseUsername.
var ErrInvalidUsername = errors.New("invalid username")

// ParseUsername lowercases s and validates it. Login input goes through it
// too, so "Alice" finds the user "alice".
func ParseUsername(s string) (Username, error) {
	s = strings.ToLower(s)
	if !usernameRE.MatchString(s) {
		return "", ErrInvalidUsername
	}
	return Username(s), nil
}

// Quota holds the per-user limits (spec §4.1).
type Quota struct {
	MaxActiveJobs          int   `json:"maxActiveJobs"`
	MaxTotalJobs           int   `json:"maxTotalJobs"`
	MaxNZBBytes            int64 `json:"maxNzbBytes"`
	MaxDownloadBytesPerDay int64 `json:"maxDownloadBytesPerDay"`
	MaxConcurrentDownloads int   `json:"maxConcurrentDownloads"`
}

// User is the Identity aggregate root. Users are created and changed only by
// the configuration sync (UC-04), never through the API.
type User struct {
	ID           UserID
	Username     Username
	PasswordHash string
	Quota        Quota
	Disabled     bool
	// ConfigVersion fingerprints the config entry the user was last synced
	// from; an unchanged fingerprint means nothing to do.
	ConfigVersion string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CanAuthenticate reports whether the user may log in or use a session.
func (u User) CanAuthenticate() bool { return !u.Disabled }

// ConfiguredUser is one user as the configuration describes them, with the
// effective quota (defaults merged with the user's override).
type ConfiguredUser struct {
	Username     Username
	PasswordHash string
	Quota        Quota
	Disabled     bool
}

// Version fingerprints the entry. Any change to hash, quota or disabled flag
// changes it.
func (c ConfiguredUser) Version() string {
	b, _ := json.Marshal(struct {
		U string `json:"u"`
		H string `json:"h"`
		Q Quota  `json:"q"`
		D bool   `json:"d"`
	}{string(c.Username), c.PasswordHash, c.Quota, c.Disabled})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// SyncPlan is the set of changes that brings the stored users in line with
// the configuration.
type SyncPlan struct {
	Create []User
	Update []User
	// RevokeSessions lists users whose sessions must end: disabled users and
	// users whose password hash changed (spec §7.2).
	RevokeSessions []UserID
}

// Empty reports whether the plan changes nothing.
func (p SyncPlan) Empty() bool {
	return len(p.Create) == 0 && len(p.Update) == 0 && len(p.RevokeSessions) == 0
}

// PlanSync compares stored users with the configured ones (UC-04): missing
// users are created, changed users updated, and users no longer configured
// are disabled, never deleted, so job ownership stays intact. It is
// idempotent: planning again after applying a plan yields an empty plan.
func PlanSync(stored []User, configured []ConfiguredUser, now time.Time, newID func() UserID) (SyncPlan, error) {
	var plan SyncPlan
	byName := make(map[Username]User, len(stored))
	for _, u := range stored {
		byName[u.Username] = u
	}
	seen := make(map[Username]bool, len(configured))
	for _, c := range configured {
		if seen[c.Username] {
			return SyncPlan{}, fmt.Errorf("user %q configured twice", c.Username)
		}
		seen[c.Username] = true
		v := c.Version()
		u, ok := byName[c.Username]
		if !ok {
			plan.Create = append(plan.Create, User{
				ID: newID(), Username: c.Username, PasswordHash: c.PasswordHash, Quota: c.Quota,
				Disabled: c.Disabled, ConfigVersion: v, CreatedAt: now, UpdatedAt: now,
			})
			continue
		}
		if u.ConfigVersion == v {
			continue
		}
		revoke := c.Disabled && !u.Disabled || c.PasswordHash != u.PasswordHash
		u.PasswordHash, u.Quota, u.Disabled, u.ConfigVersion, u.UpdatedAt = c.PasswordHash, c.Quota, c.Disabled, v, now
		plan.Update = append(plan.Update, u)
		if revoke {
			plan.RevokeSessions = append(plan.RevokeSessions, u.ID)
		}
	}
	for _, u := range stored {
		if seen[u.Username] || u.Disabled {
			continue
		}
		u.Disabled, u.ConfigVersion, u.UpdatedAt = true, "removed", now
		plan.Update = append(plan.Update, u)
		plan.RevokeSessions = append(plan.RevokeSessions, u.ID)
	}
	return plan, nil
}
