// Package domain holds the Identity context: users, credentials, sessions
// (spec §4.1).
package domain

import "errors"

// PasswordHasher hashes and verifies passwords (spec §4.5). Implementations
// encode hashes as PHC strings carrying their own parameters, so hashes made
// with older cost settings keep verifying.
type PasswordHasher interface {
	Hash(password string) (string, error)
	// Verify reports whether password matches encoded. A malformed encoded
	// hash is an error, never a match.
	Verify(password, encoded string) (bool, error)
}

// ErrMalformedHash is returned for hashes that are not valid PHC strings of
// the expected algorithm.
var ErrMalformedHash = errors.New("malformed password hash")
