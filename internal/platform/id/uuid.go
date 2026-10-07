// Package id generates UUIDv7 identifiers (RFC 9562 §5.7), used for users and
// jobs.
package id

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/feruzabad/mountenant/internal/platform/clock"
)

// Generator returns new identifiers.
type Generator interface {
	New() string
}

// UUIDv7 generates time-ordered UUIDs. IDs from one generator are strictly
// increasing, even within the same millisecond (RFC 9562 §6.2, method 3: the
// timestamp is advanced when the 12-bit counter in rand_a overflows).
type UUIDv7 struct {
	Clock clock.Clock

	mu     sync.Mutex
	lastMs int64
	seq    uint16 // 12 bits
}

func (g *UUIDv7) New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}

	now := time.Now()
	if g.Clock != nil {
		now = g.Clock.Now()
	}
	ms := now.UnixMilli()

	g.mu.Lock()
	switch {
	case ms > g.lastMs:
		g.lastMs = ms
		g.seq = uint16(b[6])<<8&0x0700 | uint16(b[7]) // random start, top bit clear leaves room to count
	default:
		g.seq++
		if g.seq > 0x0fff {
			g.lastMs++
			g.seq = 0
		}
	}
	ms, seq := g.lastMs, g.seq
	g.mu.Unlock()

	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	b[6] = 0x70 | byte(seq>>8)
	b[7] = byte(seq)
	b[8] = b[8]&0x3f | 0x80 // variant 10
	return format(b)
}

func format(b [16]byte) string {
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

// Valid reports whether s is a canonical lowercase UUID (any version). Job IDs
// arrive from URLs and backend directory names, so they are checked before
// use.
func Valid(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
				return false
			}
		}
	}
	return true
}
