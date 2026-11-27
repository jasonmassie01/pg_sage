package tuner

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// tuning-03: every work_mem hint the tuner emits or admits is at most
// min(tuner.work_mem_max_mb, 5% of host memory).

func TestWorkMemCeilingMB(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxMB     int
		host      int64
		want      int
		hostKnown bool
	}{
		{"host unknown", 512, 0, 512, false},
		{"host binds", 512, 4 << 30, 204, true},
		{"config binds", 300, 16 << 30, 300, true},
		{"equal bounds", 819, 16 << 30, 819, true},
		{"unset max uses the default", 0, 0, 512, false},
		{"negative max uses the default", -1, 64 << 30, 512, true},
		{"tiny host", 512, 10 << 20, 0, true},
		{"negative host is unknown", 512, -1, 512, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := WorkMemCeilingMB(tc.maxMB, tc.host)
			if got != tc.want || known != tc.hostKnown {
				t.Fatalf("WorkMemCeilingMB(%d, %d) = %d, %v; want %d, %v", tc.maxMB,
					tc.host, got, known, tc.want, tc.hostKnown)
			}
		})
	}
}

func TestWorkMemBudget_CapHint(t *testing.T) {
	tu := hostTuner(512, 4<<30)
	b := tu.workMemBudget()
	got, d := b.capHint(`Set(work_mem "512MB") HashJoin(a b)`)
	if got != `Set(work_mem "204MB") HashJoin(a b)` {
		t.Fatalf("capped = %q", got)
	}
	if d["requested_mb"] != 512 || d["hint_mb"] != 204 || d["ceiling_mb"] != 204 ||
		d["work_mem_max_mb"] != 512 || d["host_cap_mb"] != int64(204) ||
		d["clamped"] != true {
		t.Fatalf("detail = %v", d)
	}
	got, d = b.capHint(`Set(work_mem "64MB")`)
	if got != `Set(work_mem "64MB")` || d["clamped"] != false || d["hint_mb"] != 64 {
		t.Fatalf("under the cap: %q %v", got, d)
	}
	if got, d := b.capHint("HashJoin(a b)"); got != "HashJoin(a b)" || d != nil {
		t.Fatalf("no work_mem: %q %v", got, d)
	}
	if got, d := b.capHint(""); got != "" || d != nil {
		t.Fatalf("empty hint: %q %v", got, d)
	}
	tiny := hostTuner(512, 10<<20).workMemBudget()
	if got, d := tiny.capHint(`Set(work_mem "64MB") HashJoin(a b)`); got != "HashJoin(a b)" ||
		d["hint_mb"] != 0 {
		t.Fatalf("a host with no room drops the work_mem directive: %q %v", got, d)
	}
}

func TestProcessCandidate_StatSpillOnUnknownHostNeedsApproval(t *testing.T) {
	tu := spillTuner(newSpillReader(fourMB), 512, 0)
	ctx := context.Background()
	tu.observeSpills(ctx, []candidate{spillCand(7, appUser, 10, 0)})
	c := spillCand(7, appUser, 1_010, 1_000*2_048)
	tu.observeSpills(ctx, []candidate{c})
	fs := tu.processCandidate(ctx, c)
	if len(fs) != 1 {
		t.Fatalf("findings = %+v", fs)
	}
	f := fs[0]
	if f.Detail["hint_directive"] != `Set(work_mem "32MB")` ||
		f.RecommendedSQL != BuildInsertSQL(7, `Set(work_mem "32MB")`) {
		t.Fatalf("finding = %+v", f)
	}
	why, _ := f.Detail[analyzer.DetailApprovalRequired].(string)
	if !strings.Contains(why, "host memory is unknown") {
		t.Fatalf("approval_required = %q", why)
	}
	sz, _ := f.Detail["stat_spill_sizing"].(map[string]any)
	if sz["per_call_spill_kb"] != int64(16_384) || sz["hint_mb"] != 32 {
		t.Fatalf("sizing detail = %v", f.Detail["stat_spill_sizing"])
	}
}

func TestProcessCandidate_KnownHostNeedsNoApproval(t *testing.T) {
	tu := spillTuner(newSpillReader(fourMB), 512, 16<<30)
	ctx := context.Background()
	tu.observeSpills(ctx, []candidate{spillCand(7, appUser, 10, 0)})
	c := spillCand(7, appUser, 1_010, 1_000*2_048)
	tu.observeSpills(ctx, []candidate{c})
	fs := tu.processCandidate(ctx, c)
	if len(fs) != 1 {
		t.Fatalf("findings = %+v", fs)
	}
	if _, gated := fs[0].Detail[analyzer.DetailApprovalRequired]; gated {
		t.Fatalf("a known host within the cap needs no approval: %v", fs[0].Detail)
	}
	b, _ := fs[0].Detail["work_mem_budget"].(map[string]any)
	if b["ceiling_mb"] != 512 || b["host_cap_mb"] != int64(819) {
		t.Fatalf("budget = %v", fs[0].Detail["work_mem_budget"])
	}
}

func TestProcessCandidate_NoSpillNoFinding(t *testing.T) {
	// No concurrent access here: processCandidate runs under Tune's lock;
	// TestStatSpill_ConcurrentTuneAndCheckHint covers the shared state.
	tu := spillTuner(newSpillReader(fourMB), 512, 16<<30)
	c := spillCand(7, appUser, 1_010, 1_000*2_048)
	if fs := tu.processCandidate(context.Background(), c); len(fs) != 0 {
		t.Fatalf("cumulative counters alone give no finding: %+v", fs)
	}
}
