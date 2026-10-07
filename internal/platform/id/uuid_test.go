package id

import (
	"testing"
	"time"

	"github.com/feruzabad/mountenant/internal/platform/clock"
)

func TestUUIDv7Format(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	g := &UUIDv7{Clock: c}
	s := g.New()
	if !Valid(s) {
		t.Fatalf("invalid: %s", s)
	}
	if s[14] != '7' {
		t.Fatalf("version nibble %c in %s", s[14], s)
	}
	if v := s[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
		t.Fatalf("variant nibble %c in %s", v, s)
	}
	// 48-bit big-endian Unix milliseconds.
	if got, want := s[:8]+s[9:13], "01a1163c1a00"; got != want {
		t.Fatalf("timestamp %s, want %s", got, want)
	}
}

func TestUUIDv7Monotonic(t *testing.T) {
	g := &UUIDv7{Clock: clock.NewFake(time.Unix(1_800_000_000, 0))}
	prev := g.New()
	for i := 0; i < 20_000; i++ { // crosses several counter overflows
		s := g.New()
		if s <= prev {
			t.Fatalf("not increasing at %d: %s <= %s", i, s, prev)
		}
		prev = s
	}
}

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"0192f0e0-0000-7000-8000-000000000001":  true,
		"0192F0E0-0000-7000-8000-000000000001":  false,
		"0192f0e0-0000-7000-8000-00000000000":   false,
		"0192f0e0-0000-7000-8000-0000000000011": false,
		"0192f0e0x0000-7000-8000-000000000001":  false,
		"../../../../../../../../../../../../.": false,
	} {
		if Valid(s) != want {
			t.Errorf("Valid(%q) = %v", s, !want)
		}
	}
}
