package logging

import (
	"bytes"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var fixed = func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.FixedZone("x", 7200)) }

// lineRE is the documented line layout; deploy/fail2ban filters rely on it.
var lineRE = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ mountenant event=[a-z0-9_]+ ip=\S+ user="(?:[^"\\]|\\.)*" reason=[a-z0-9_-]+\n$`)

func TestFormat(t *testing.T) {
	l := &SecurityLog{now: fixed}
	got := l.Format(SecurityEvent{Name: EventAuthFailure, IP: netip.MustParseAddr("::ffff:203.0.113.9"), User: "alice", Reason: "bad_credentials"})
	want := "2026-10-07T10:00:00Z mountenant event=auth_failure ip=203.0.113.9 user=\"alice\" reason=bad_credentials\n"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestFormatResistsInjection(t *testing.T) {
	l := &SecurityLog{now: fixed}
	for _, e := range []SecurityEvent{
		{Name: EventAuthFailure, User: "x\" reason=ok\n2026-10-07T10:00:00Z mountenant event=auth_success ip=1.2.3.4 user=\"root", Reason: "bad"},
		{Name: "auth failure\n", User: "\x1b[31mred\t\r", Reason: "a b"},
		{Name: EventLinkInvalid, User: strings.Repeat("é", 500), Reason: "expired"},
		{Name: EventForbiddenPeer, User: `\"`, Reason: ""},
	} {
		line := l.Format(e)
		if strings.Count(line, "\n") != 1 || !lineRE.MatchString(line) {
			t.Errorf("unsafe line: %q", line)
		}
	}
}

func TestLogAppendsAndReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "security.log")
	var stdout bytes.Buffer
	l, err := OpenSecurityLog(path, &stdout, fixed)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ev := SecurityEvent{Name: EventAuthSuccess, IP: netip.MustParseAddr("198.51.100.7"), User: "bob", Reason: "ok"}
	l.Log(ev)

	// logrotate: move the file away, then SIGHUP.
	os.Rename(path, path+".1")
	if err := l.Reopen(); err != nil {
		t.Fatal(err)
	}
	l.Log(ev)

	for _, p := range []string{path + ".1", path} {
		b, _ := os.ReadFile(p)
		if strings.Count(string(b), "\n") != 1 {
			t.Errorf("%s: %q", p, b)
		}
	}
	if strings.Count(stdout.String(), "event=auth_success") != 2 {
		t.Errorf("stdout: %q", stdout.String())
	}
}

func TestNew(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "warn", "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), `"msg":"shown"`) {
		t.Fatalf("%q", buf.String())
	}
	if _, err := New(&buf, "info", "xml"); err == nil {
		t.Fatal("bad format accepted")
	}
}
