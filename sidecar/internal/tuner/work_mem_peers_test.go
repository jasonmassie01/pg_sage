package tuner

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// tuning-03 review: a pg_hint_plan hint applies to every role that runs the
// statement, so a stat-spill hint sized for one role must not lower another
// role's work_mem; and the tuner's 64 MB floor must not turn an agent
// request within the host cap into a memory refusal.

func TestStatSpill_HintNeverLowersAnotherRolesWorkMem(t *testing.T) {
	// The app role (4 MB) spills 16 MB per call: alone it gets 32 MB.
	cases := map[string]struct {
		otherKB  int64
		known    bool
		otherQID int64
		want     string
	}{
		"other role at 64MB":          {64 * 1024, true, 7, ""},
		"other role at 33MB":          {33 * 1024, true, 7, ""},
		"other role at exactly 32MB":  {32 * 1024, true, 7, `Set(work_mem "32MB")`},
		"other role at 4MB":           {fourMB, true, 7, `Set(work_mem "32MB")`},
		"other role unknown":          {0, false, 7, ""},
		"64MB role on another query":  {64 * 1024, true, 8, `Set(work_mem "32MB")`},
		"unknown role on other query": {0, false, 8, `Set(work_mem "32MB")`},
	}
	ctx := context.Background()
	for name, tc := range cases {
		r := newSpillReader(fourMB)
		if tc.known {
			r.settings.WorkMemKB[otherUser] = tc.otherKB
		}
		tu := spillTuner(r, 512, 16<<30)
		tu.observeSpills(ctx, []candidate{spillCand(7, appUser, 10, 0),
			spillCand(tc.otherQID, otherUser, 10, 0)})
		cur := []candidate{spillCand(7, appUser, 1_010, 1_000*2_048),
			spillCand(tc.otherQID, otherUser, 20, 0)}
		tu.observeSpills(ctx, cur)
		got := CombineHints(tu.prescribeAll(tu.gatherSymptoms(ctx, cur[0])))
		if got != tc.want {
			t.Errorf("%s: hint = %q, want %q", name, got, tc.want)
		}
		wantUsers := []int64{appUser}
		if tc.otherQID == 7 {
			wantUsers = []int64{appUser, otherUser}
		}
		if len(r.users) != 1 || !slices.Equal(r.users[0], wantUsers) {
			t.Errorf("%s: settings read for %v, want %v", name, r.users, wantUsers)
		}
	}
}

func TestCheckHint_FloorDoesNotRefuseARequestWithinTheCap(t *testing.T) {
	// 5% of 1 GiB is 51 MB, below the 64 MB floor of hint work_mem.
	ctx := context.Background()
	tu := hostTuner(512, 1<<30)
	for _, req := range []string{"8MB", "51MB", "4096kB"} {
		f, err := tu.CheckHint(ctx, agentHint(`Set(work_mem "`+req+`") HashJoin(o c)`))
		if err != nil {
			t.Fatalf("%s on a 1 GiB host: %v", req, err)
		}
		if got := f.Detail["hint_directive"]; got != `Set(work_mem "51MB") HashJoin(o c)` {
			t.Fatalf("%s: hint = %v, want the floor clamped to the 51 MB cap", req, got)
		}
	}
	for _, req := range []string{"52MB", "512MB", "1GB"} {
		_, err := tu.CheckHint(ctx, agentHint(`Set(work_mem "`+req+`")`))
		if !errors.Is(err, ErrHintMemory) {
			t.Fatalf("%s on a 1 GiB host: err = %v, want a memory refusal", req, err)
		}
	}
	if err := tu.RecordHint(ctx, agentHint(`Set(work_mem "8MB")`)); err != nil {
		t.Fatalf("record 8MB: %v", err)
	}
	if _, cooling := tu.recentlyTuned[4242]; !cooling {
		t.Fatalf("a recorded hint cools its statement down: %v", tu.recentlyTuned)
	}
	// A tiny host (5% under 1 MB) leaves no room for any work_mem hint.
	if _, err := hostTuner(512, 16<<20).CheckHint(ctx,
		agentHint(`Set(work_mem "1MB")`)); !errors.Is(err, ErrHintMemory) {
		t.Fatalf("1MB on a 16 MiB host: err = %v, want a memory refusal", err)
	}
}
