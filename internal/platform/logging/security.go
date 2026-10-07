package logging

import (
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Security event names (spec §7.8). The line format is a public contract:
// fail2ban filters in deploy/fail2ban match it.
const (
	EventAuthFailure     = "auth_failure"
	EventAuthSuccess     = "auth_success"
	EventAuthRateLimited = "auth_rate_limited"
	EventCSRFFailure     = "csrf_failure"
	EventLinkInvalid     = "link_invalid"
	EventForbiddenPeer   = "forbidden_peer"
)

// MaxUsernameLen caps logged usernames; longer input is truncated.
const MaxUsernameLen = 64

// SecurityEvent is one line of the security log.
type SecurityEvent struct {
	Name   string     // one of the Event* constants
	IP     netip.Addr // real client IP, resolved through the trusted proxies
	User   string     // as typed by the client; sanitised before writing
	Reason string     // short machine code, e.g. "bad_password"
}

// SecurityLog writes SecurityEvents as single plain-text lines:
//
//	2026-10-07T12:00:00Z mountenant event=auth_failure ip=203.0.113.9 user="alice" reason=bad_credentials
//
// The file is opened in append mode and can be reopened after logrotate moved
// it (Reopen, called on SIGHUP). It is safe for concurrent use.
type SecurityLog struct {
	path   string
	stdout io.Writer
	now    func() time.Time

	mu   sync.Mutex
	file *os.File
}

// OpenSecurityLog opens path for appending (if not empty) and mirrors lines
// to stdout (if not nil).
func OpenSecurityLog(path string, stdout io.Writer, now func() time.Time) (*SecurityLog, error) {
	l := &SecurityLog{path: path, stdout: stdout, now: now}
	if l.now == nil {
		l.now = time.Now
	}
	if err := l.Reopen(); err != nil {
		return nil, err
	}
	return l, nil
}

// Reopen closes and reopens the log file.
func (l *SecurityLog) Reopen() error {
	if l.path == "" {
		return nil
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("security log: %w", err)
	}
	l.mu.Lock()
	old := l.file
	l.file = f
	l.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

// Close closes the log file.
func (l *SecurityLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Log writes one event. Write errors are returned but callers usually only
// count them: a full disk must not block logins.
func (l *SecurityLog) Log(e SecurityEvent) error {
	line := l.Format(e)
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	if l.file != nil {
		_, err = io.WriteString(l.file, line)
	}
	if l.stdout != nil {
		_, _ = io.WriteString(l.stdout, line) // the file is authoritative
	}
	return err
}

// Format renders e as one line, including the trailing newline.
func (l *SecurityLog) Format(e SecurityEvent) string {
	ip := "-"
	if e.IP.IsValid() {
		ip = e.IP.Unmap().String()
	}
	return fmt.Sprintf("%s mountenant event=%s ip=%s user=\"%s\" reason=%s\n",
		l.now().UTC().Format(time.RFC3339), token(e.Name), ip, sanitizeUser(e.User), token(e.Reason))
}

// token restricts a name or reason code to [a-z0-9_]; anything else becomes
// "_" so a value can never break the line format.
func token(s string) string {
	if s == "" {
		return "-"
	}
	b := []byte(s)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			b[i] = '_'
		}
	}
	return string(b)
}

// sanitizeUser strips control and non-printable characters, escapes quotes
// and backslashes and caps the length, so client input cannot forge lines
// (log injection).
func sanitizeUser(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == MaxUsernameLen {
			break
		}
		if !unicode.IsPrint(r) || unicode.IsSpace(r) && r != ' ' {
			continue
		}
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
