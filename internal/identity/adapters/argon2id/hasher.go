// Package argon2id implements the PasswordHasher port with argon2id
// (RFC 9106) and PHC string encoding.
package argon2id

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/feruzabad/mountenant/internal/identity/domain"
)

const (
	saltLen = 16
	keyLen  = 32

	// Upper bounds for parameters read from a hash, so a typo in users.json
	// cannot make one login allocate gigabytes.
	maxMemoryKiB  = 1 << 20 // 1 GiB
	maxIterations = 64
	maxSaltLen    = 64
	maxKeyLen     = 64
)

// Params are the argon2id cost parameters used for new hashes.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// RFC9106 is the RFC 9106 second recommended option (m=64 MiB, t=3, p=4),
// the spec's default (§7.1).
var RFC9106 = Params{MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 4}

// Hasher hashes with Params and verifies any argon2id PHC string.
type Hasher struct {
	Params Params
}

var _ domain.PasswordHasher = Hasher{}

var b64 = base64.RawStdEncoding

func (h Hasher) Hash(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	p := h.Params
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func (Hasher) Verify(password, encoded string) (bool, error) {
	p, salt, key, err := Decode(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB, p.Parallelism, uint32(len(key))) //nolint:gosec // len(key) <= maxKeyLen (Decode)
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

// Decode parses $argon2id$v=19$m=<kib>,t=<iter>,p=<par>$<salt>$<key>.
func Decode(encoded string) (p Params, salt, key []byte, err error) {
	bad := func(why string) (Params, []byte, []byte, error) {
		return Params{}, nil, nil, fmt.Errorf("%w: %s", domain.ErrMalformedHash, why)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return bad("not an argon2id PHC string")
	}
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return bad("unsupported version")
	}
	var m, t, par uint64
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, _ := strings.Cut(kv, "=")
		n, perr := strconv.ParseUint(v, 10, 32)
		if perr != nil {
			return bad("bad parameter " + k)
		}
		switch k {
		case "m":
			m = n
		case "t":
			t = n
		case "p":
			par = n
		default:
			return bad("unknown parameter " + k)
		}
	}
	switch {
	case m < 8*par || m > maxMemoryKiB:
		return bad("memory out of range")
	case t < 1 || t > maxIterations:
		return bad("iterations out of range")
	case par < 1 || par > 255:
		return bad("parallelism out of range")
	}
	// Length checks run on the encoded form first, so an oversized value is
	// rejected before it is decoded.
	if len(parts[4]) > b64.EncodedLen(maxSaltLen) || len(parts[5]) > b64.EncodedLen(maxKeyLen) {
		return bad("salt or key too long")
	}
	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) < 8 {
		return bad("bad salt")
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) < 16 {
		return bad("bad key")
	}
	return Params{MemoryKiB: uint32(m), Iterations: uint32(t), Parallelism: uint8(par)}, salt, key, nil
}
