package domain

import (
	"bytes"
	"testing"
	"time"
)

var policy = SessionPolicy{IdleTimeout: 12 * time.Hour, AbsoluteTimeout: 7 * 24 * time.Hour}

func TestNewSessionAndParseToken(t *testing.T) {
	s, token, err := NewSession("u1", t0, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 43 {
		t.Fatalf("token length %d", len(token))
	}
	id, err := ParseToken(token)
	if err != nil || id != s.ID {
		t.Fatalf("token does not map to the session: %v", err)
	}
	if bytes.Contains(s.ID[:], []byte(token)) {
		t.Fatal("raw token stored")
	}
	if !s.ExpiresAt.Equal(t0.Add(policy.AbsoluteTimeout)) {
		t.Fatalf("expires %v", s.ExpiresAt)
	}
	other, token2, _ := NewSession("u1", t0, policy, nil)
	if token == token2 || bytes.Equal(other.CSRFSecret, s.CSRFSecret) {
		t.Fatal("tokens not random")
	}
	for _, bad := range []string{"", "short", token + "A", token[:42] + "!"} {
		if _, err := ParseToken(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSessionActive(t *testing.T) {
	s, _, _ := NewSession("u1", t0, policy, nil)
	cases := []struct {
		lastSeen, now time.Duration
		want          bool
	}{
		{0, 0, true},
		{0, 12*time.Hour - time.Second, true},
		{0, 12 * time.Hour, false}, // idle
		{6 * 24 * time.Hour, 6*24*time.Hour + time.Hour, true},
		{7*24*time.Hour - time.Minute, 7 * 24 * time.Hour, false}, // absolute
	}
	for _, c := range cases {
		s.LastSeenAt = t0.Add(c.lastSeen)
		if got := s.Active(t0.Add(c.now), policy); got != c.want {
			t.Errorf("lastSeen +%v now +%v: %v", c.lastSeen, c.now, got)
		}
	}
}

func TestCSRF(t *testing.T) {
	s, _, _ := NewSession("u1", t0, policy, nil)
	if !s.CheckCSRF(s.CSRFToken()) {
		t.Fatal("own token rejected")
	}
	other, _, _ := NewSession("u1", t0, policy, nil)
	for _, bad := range []string{"", other.CSRFToken(), s.CSRFToken()[:42]} {
		if s.CheckCSRF(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if (Session{}).CheckCSRF((Session{}).CSRFToken()) {
		t.Fatal("empty secret accepted")
	}
}
