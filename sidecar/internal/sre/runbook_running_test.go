package sre

import (
	"testing"
	"time"
)

// The running-version rule (playbooks-10) without a database: settle walks
// the versions newest first, skips unsigned and tampered ones, runs the
// newest validly signed version only while it still validates, and never
// falls back past a validly signed version that no longer validates.

// No concurrent access tests: runningVersion and settle are pure functions
// over values the caller owns.

func signedV(version int, valid bool, desc string) RunbookVersion {
	at := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	d := idleRunbook()
	d.Description = desc
	return RunbookVersion{Version: version, Name: d.Name, Definition: d, SignedAt: &at,
		SignedBy: "user:1", SignatureValid: valid}
}

func unsignedV(version int) RunbookVersion {
	d := idleRunbook()
	return RunbookVersion{Version: version, Name: d.Name, Definition: d}
}

// noLongerValid is a version an admin signed that today's catalogs refuse.
func noLongerValid(version int) RunbookVersion {
	v := signedV(version, true, "stale")
	v.Definition.Nodes = append(v.Definition.Nodes[:0:0], v.Definition.Nodes...)
	v.Definition.Nodes[0].Probe = "no_such_probe"
	return v
}

func TestRunningVersion_Rule(t *testing.T) {
	cases := []struct {
		name     string
		versions []RunbookVersion
		want     int
	}{
		{"nil", nil, 0},
		{"empty", []RunbookVersion{}, 0},
		{"only an unsigned draft", []RunbookVersion{unsignedV(1)}, 0},
		{"signed v1", []RunbookVersion{signedV(1, true, "")}, 1},
		{"draft over signed v1", []RunbookVersion{unsignedV(2), signedV(1, true, "")}, 1},
		{"two drafts over signed v1", []RunbookVersion{unsignedV(3), unsignedV(2),
			signedV(1, true, "")}, 1},
		{"newest signed wins", []RunbookVersion{signedV(2, true, "v2"),
			signedV(1, true, "")}, 2},
		{"tampered v2 falls back to v1", []RunbookVersion{signedV(2, false, "v2"),
			signedV(1, true, "")}, 1},
		{"every signature tampered", []RunbookVersion{signedV(2, false, "v2"),
			signedV(1, false, "")}, 0},
		{"newest valid no longer validates: no fallback", []RunbookVersion{
			noLongerValid(2), signedV(1, true, "")}, 0},
		{"draft over a version that no longer validates", []RunbookVersion{
			unsignedV(3), noLongerValid(2), signedV(1, true, "")}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runningVersion(c.versions)
			switch {
			case c.want == 0 && got != nil:
				t.Fatalf("running = v%d, want nothing running", got.Version)
			case c.want != 0 && (got == nil || got.Version != c.want ||
				!got.SignatureValid || got.SignedAt == nil):
				t.Fatalf("running = %+v, want signed v%d", got, c.want)
			}
		})
	}
}

func TestSettle_StatusAndRunningVersion(t *testing.T) {
	v1, v2 := signedV(1, true, ""), unsignedV(2)
	rb := Runbook{LatestVersion: 2, Latest: v2}
	rb.settle([]RunbookVersion{v2, v1})
	if rb.Status != RunbookDraft || !rb.Runnable || rb.RunningVersion != 1 ||
		rb.Running == nil || rb.Running.Version != 1 {
		t.Fatalf("staged = %+v; want status draft with v1 running", rb)
	}
	// A stale Running from an earlier settle never survives a re-settle.
	rb.settle(nil)
	if rb.Runnable || rb.Running != nil || rb.RunningVersion != 0 {
		t.Fatalf("re-settled without signed versions = %+v; want nothing running", rb)
	}
	retiredAt := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	retired := Runbook{LatestVersion: 1, Latest: v1, RetiredAt: &retiredAt}
	retired.settle([]RunbookVersion{v1})
	if retired.Status != RunbookRetired || retired.Runnable || retired.Running != nil ||
		retired.RunningVersion != 0 {
		t.Fatalf("retired = %+v; a retired runbook runs no version", retired)
	}
}

func TestPickRunbook_NeverRunsWithoutARunningVersion(t *testing.T) {
	d := idleRunbook()
	// A hand-built Runnable runbook without Running must not run its Latest.
	rb := Runbook{ID: NewUUID(), Runnable: true, Latest: RunbookVersion{Name: d.Name,
		Definition: d}}
	if got, ok := pickRunbook([]Runbook{rb}, TriggerLock,
		[]string{"idle_in_tx_holder"}); ok {
		t.Fatalf("picked %+v; a runbook without a running version never runs", got)
	}
	// The running version's trigger decides, not the draft's.
	other := idleRunbook()
	other.Trigger.Kinds = []string{"no_such_kind"}
	running := RunbookVersion{Version: 1, Name: d.Name, Definition: d}
	staged := Runbook{ID: NewUUID(), Runnable: true, Running: &running,
		Latest: RunbookVersion{Version: 2, Name: d.Name, Definition: other}}
	got, ok := pickRunbook([]Runbook{staged}, TriggerLock, []string{"idle_in_tx_holder"})
	if !ok || got.ID != staged.ID || got.Running.Version != 1 {
		t.Fatalf("picked %+v, %v; want the staged runbook by its running v1", got, ok)
	}
}
