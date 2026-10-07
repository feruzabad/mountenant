package domain

import (
	"errors"
	"testing"
	"time"
)

var (
	t0  = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ret = Retention{Ready: 168 * time.Hour, Failed: 24 * time.Hour, ImportTimeout: 30 * time.Minute}
)

func newJob() *Job { return NewJob("j1", "u1", "../x/Debian.nzb", [32]byte{1}, t0, ret) }

func types(ev []Event) []EventType {
	var out []EventType
	for _, e := range ev {
		out = append(out, e.Type)
	}
	return out
}

func TestHappyPath(t *testing.T) {
	j := newJob()
	if j.Status != StatusQueued || j.NZBName != "Debian.nzb" || !j.ExpiresAt.Equal(t0.Add(ret.ImportTimeout+ret.Failed)) {
		t.Fatalf("new job: %+v", j)
	}
	if err := j.Accepted(BackendRef{JobID: "j1", NzoID: "n"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := j.StartImport(t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	files := []JobFile{{RelPath: "debian.iso", Size: 10, ContentType: "x"}, {RelPath: "sub/readme.txt", Size: 1}}
	if err := j.MarkReady(files, 10, t0.Add(2*time.Minute), ret); err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusReady || !j.ExpiresAt.Equal(t0.Add(2*time.Minute+ret.Ready)) || len(j.Files) != 2 {
		t.Fatalf("ready: %+v", j)
	}
	if f, ok := j.File("sub/readme.txt"); !ok || f.Size != 1 {
		t.Fatal("File lookup")
	}
	got := types(j.Events())
	want := []EventType{EventSubmitted, EventImporting, EventReady}
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v", got)
		}
	}
	if len(j.Events()) != 0 {
		t.Fatal("events not cleared")
	}
}

func TestTransitions(t *testing.T) {
	ready := func() *Job {
		j := newJob()
		j.MarkReady([]JobFile{{RelPath: "a", Size: 1}}, 10, t0, ret) // Queued → Ready directly is allowed
		return j
	}
	// Content gone: Ready → Failed clears the catalogue.
	j := ready()
	if err := j.Fail(Failure{Code: FailContentMissing}, t0.Add(time.Hour), ret); err != nil {
		t.Fatal(err)
	}
	if len(j.Files) != 0 || !j.ExpiresAt.Equal(t0.Add(time.Hour+ret.Failed)) || j.Failure.Code != FailContentMissing {
		t.Fatalf("failed: %+v", j)
	}
	// Failed is stable until deletion.
	for name, err := range map[string]error{
		"start":  j.StartImport(t0),
		"ready":  j.MarkReady([]JobFile{{RelPath: "a"}}, 10, t0, ret),
		"fail":   j.Fail(Failure{}, t0, ret),
		"accept": j.Accepted(BackendRef{}, t0),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("failed → %s: %v", name, err)
		}
	}
	if err := j.Delete(t0); err != nil || j.Status != StatusDeleted || !j.ExpiresAt.IsZero() {
		t.Fatalf("delete: %v %+v", err, j)
	}
	if err := j.Delete(t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("deleted twice")
	}
	if j.Expired(t0.Add(1000 * time.Hour)) {
		t.Fatal("deleted job reported as expired")
	}
	j = ready()
	if err := j.StartImport(t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("ready → importing allowed")
	}
}

func TestMarkReadyValidation(t *testing.T) {
	for name, c := range map[string]struct {
		files []JobFile
		want  error
	}{
		"none":      {nil, ErrNoFiles},
		"too many":  {[]JobFile{{RelPath: "a"}, {RelPath: "b"}, {RelPath: "c"}}, ErrTooManyFiles},
		"traversal": {[]JobFile{{RelPath: "../etc/passwd"}}, ErrBadRelPath},
		"duplicate": {[]JobFile{{RelPath: "a"}, {RelPath: "a"}}, ErrBadRelPath},
		"negative":  {[]JobFile{{RelPath: "a", Size: -1}}, ErrBadRelPath},
	} {
		j := newJob()
		if err := j.MarkReady(c.files, 2, t0, ret); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if j.Status != StatusQueued {
			t.Errorf("%s: status changed to %s", name, j.Status)
		}
	}
}

func TestTimeoutAndExpiry(t *testing.T) {
	j := newJob()
	if j.ImportTimedOut(t0.Add(29*time.Minute), ret) || !j.ImportTimedOut(t0.Add(30*time.Minute), ret) {
		t.Fatal("import timeout")
	}
	if j.Expired(t0.Add(ret.ImportTimeout+ret.Failed-time.Second)) || !j.Expired(t0.Add(ret.ImportTimeout+ret.Failed)) {
		t.Fatal("queued expiry")
	}
}

func TestValidRelPath(t *testing.T) {
	for p, want := range map[string]bool{
		"a.iso": true, "dir/sub/a b#%?.mkv": true, "ünï/çødé.txt": true,
		"": false, "/abs": false, "a/../b": false, "..": false, "./a": false,
		"a//b": false, "a/": false, "a\\b": false, "a\x00b": false,
	} {
		if ValidRelPath(p) != want {
			t.Errorf("%q: %v", p, !want)
		}
	}
}

func TestCheckSubmission(t *testing.T) {
	l := Limits{MaxActiveJobs: 2, MaxTotalJobs: 3}
	if err := CheckSubmission(l, Counts{Active: 1, Total: 2}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Counts{{Active: 2, Total: 2}, {Active: 0, Total: 3}} {
		if err := CheckSubmission(l, c); !errors.Is(err, ErrQuotaExceeded) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if EffectiveNZBLimit(100, 50) != 50 || EffectiveNZBLimit(10, 50) != 10 {
		t.Fatal("hard cap")
	}
}
