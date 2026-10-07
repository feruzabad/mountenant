package logging

import (
	"net/netip"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

const deployDir = "../../../deploy/fail2ban/"

// sampleEvents produce deploy/fail2ban/sample.log, which CI feeds to the
// real fail2ban-regex.
var sampleEvents = []SecurityEvent{
	{Name: EventAuthFailure, IP: netip.MustParseAddr("203.0.113.9"), User: "alice", Reason: "bad_credentials"},
	{Name: EventAuthSuccess, IP: netip.MustParseAddr("203.0.113.9"), User: "alice", Reason: "ok"},
	{Name: EventLinkInvalid, IP: netip.MustParseAddr("2001:db8::7"), Reason: "expired"},
	{Name: EventAuthFailure, IP: netip.MustParseAddr("198.51.100.4"), User: `x" reason=ok ip=1.2.3.4`, Reason: "bad_credentials"},
	{Name: EventAuthRateLimited, IP: netip.MustParseAddr("203.0.113.9"), User: "alice", Reason: "throttled"},
	{Name: EventCSRFFailure, IP: netip.MustParseAddr("203.0.113.9"), User: "alice", Reason: "origin"},
	{Name: EventForbiddenPeer, IP: netip.MustParseAddr("192.0.2.1"), Reason: "untrusted_peer"},
}

func sampleLines() []string {
	var out []string
	for i, e := range sampleEvents {
		l := &SecurityLog{now: func() time.Time { return time.Date(2026, 10, 7, 10, 0, i, 0, time.UTC) }}
		out = append(out, l.Format(e))
	}
	return out
}

func TestSampleLogIsCurrent(t *testing.T) {
	b, err := os.ReadFile(deployDir + "sample.log")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(sampleLines(), ""); string(b) != want {
		t.Fatalf("deploy/fail2ban/sample.log is stale; it must be:\n%s", want)
	}
}

// TestFilterMatchesEmittedLines applies the filter's failregex the way
// fail2ban does: the date is cut off first, <HOST> captures the address.
func TestFilterMatchesEmittedLines(t *testing.T) {
	conf, err := os.ReadFile(deployDir + "filter.d/mountenant.conf")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^failregex = (.+)$`).FindSubmatch(conf)
	if m == nil {
		t.Fatal("no failregex")
	}
	re := regexp.MustCompile(strings.Replace(string(m[1]), "<HOST>", `(?P<host>\S+)`, 1))

	banned := map[string]bool{EventAuthFailure: true, EventLinkInvalid: true}
	for i, line := range sampleLines() {
		e := sampleEvents[i]
		rest := strings.TrimSuffix(line[len("2026-10-07T10:00:00Z"):], "\n")
		sm := re.FindStringSubmatch(rest)
		if (sm != nil) != banned[e.Name] {
			t.Errorf("%s: matched=%v", e.Name, sm != nil)
			continue
		}
		if sm != nil && sm[1] != e.IP.String() {
			t.Errorf("%s: captured host %q, want %s", e.Name, sm[1], e.IP)
		}
	}
}
