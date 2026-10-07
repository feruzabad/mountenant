package domain

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func ids() func() UserID {
	n := 0
	return func() UserID { n++; return UserID(fmt.Sprintf("id-%d", n)) }
}

func TestParseUsername(t *testing.T) {
	for in, want := range map[string]string{"alice": "alice", "Alice": "alice", "a.b-c_9": "a.b-c_9"} {
		if got, err := ParseUsername(in); err != nil || string(got) != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "ab", "with space", "ünï", "x\n", string(make([]byte, 33))} {
		if _, err := ParseUsername(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

// apply simulates a repository applying the plan.
func apply(stored []User, p SyncPlan) []User {
	byID := map[UserID]int{}
	for i, u := range stored {
		byID[u.ID] = i
	}
	out := append([]User(nil), stored...)
	for _, u := range p.Update {
		out[byID[u.ID]] = u
	}
	return append(out, p.Create...)
}

func TestPlanSync(t *testing.T) {
	q := Quota{MaxActiveJobs: 1, MaxTotalJobs: 2, MaxNZBBytes: 3, MaxDownloadBytesPerDay: 4, MaxConcurrentDownloads: 5}
	alice := ConfiguredUser{Username: "alice", PasswordHash: "h1", Quota: q}
	bob := ConfiguredUser{Username: "bob", PasswordHash: "h2", Quota: q}
	newID := ids()

	// Initial sync creates everyone.
	p, err := PlanSync(nil, []ConfiguredUser{alice, bob}, t0, newID)
	if err != nil || len(p.Create) != 2 || len(p.Update) != 0 {
		t.Fatalf("initial: %+v %v", p, err)
	}
	stored := apply(nil, p)

	// Idempotent.
	if p, _ := PlanSync(stored, []ConfiguredUser{alice, bob}, t0, newID); !p.Empty() {
		t.Fatalf("second sync not empty: %+v", p)
	}

	// Quota change: update without revoking sessions.
	alice2 := alice
	alice2.Quota.MaxActiveJobs = 9
	p, _ = PlanSync(stored, []ConfiguredUser{alice2, bob}, t0.Add(time.Hour), newID)
	if len(p.Update) != 1 || p.Update[0].Quota.MaxActiveJobs != 9 || len(p.RevokeSessions) != 0 || !p.Update[0].UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("quota change: %+v", p)
	}
	stored = apply(stored, p)

	// Password change revokes sessions.
	bob2 := bob
	bob2.PasswordHash = "h3"
	p, _ = PlanSync(stored, []ConfiguredUser{alice2, bob2}, t0, newID)
	if len(p.RevokeSessions) != 1 || p.RevokeSessions[0] != stored[1].ID {
		t.Fatalf("password change: %+v", p)
	}
	stored = apply(stored, p)

	// Removal disables and revokes; the user is never deleted.
	p, _ = PlanSync(stored, []ConfiguredUser{alice2}, t0, newID)
	if len(p.Update) != 1 || !p.Update[0].Disabled || len(p.RevokeSessions) != 1 || len(p.Create) != 0 {
		t.Fatalf("removal: %+v", p)
	}
	stored = apply(stored, p)
	if p, _ := PlanSync(stored, []ConfiguredUser{alice2}, t0, newID); !p.Empty() {
		t.Fatalf("removed user disabled again: %+v", p)
	}

	// Re-adding re-enables with the same ID, so their jobs stay theirs.
	p, _ = PlanSync(stored, []ConfiguredUser{alice2, bob2}, t0, newID)
	if len(p.Update) != 1 || p.Update[0].Disabled || p.Update[0].ID != stored[1].ID || len(p.RevokeSessions) != 0 {
		t.Fatalf("re-add: %+v", p)
	}
	stored = apply(stored, p)

	// Disabled in config: disable and revoke.
	bob3 := bob2
	bob3.Disabled = true
	p, _ = PlanSync(stored, []ConfiguredUser{alice2, bob3}, t0, newID)
	if len(p.Update) != 1 || !p.Update[0].Disabled || len(p.RevokeSessions) != 1 {
		t.Fatalf("disable: %+v", p)
	}

	if _, err := PlanSync(nil, []ConfiguredUser{alice, alice}, t0, newID); err == nil {
		t.Fatal("duplicate configured user accepted")
	}
}
