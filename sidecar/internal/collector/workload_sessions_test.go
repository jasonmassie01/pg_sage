package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// market-08 without a database: workload_sessions is a bounded extra
// category read on every server version, unknown (not empty) when it
// fails, and sqlcommenter tags come from long-running active queries only.
//
// No concurrent access tests: the steps run sequentially on the
// collector's loop goroutine.

// Bounded probe: every statement is tagged as pg_sage's own, leaves
// pg_sage's sessions out and is capped by a LIMIT.
func TestWorkloadSessionsSQL_IsBoundedAndExcludesSelf(t *testing.T) {
	for name, sql := range map[string]string{
		"groups": workloadGroupsSQL, "long": workloadLongSQL,
		"roles": workloadRolesSQL(testPgss.view), "cluster": workloadClusterSessionsSQL,
	} {
		if !strings.HasPrefix(sql, sageTag) {
			t.Errorf("%s SQL is not tagged as pg_sage's own statement", name)
		}
		low := strings.ToLower(sql)
		if !strings.Contains(low, "limit ") {
			t.Errorf("%s SQL has no LIMIT", name)
		}
	}
	for name, sql := range map[string]string{
		"groups": workloadGroupsSQL, "long": workloadLongSQL,
	} {
		if !strings.Contains(sql, notSelf) || !strings.Contains(sql, "'client backend'") {
			t.Errorf("%s SQL does not keep to client sessions other than pg_sage's", name)
		}
		if !strings.Contains(sql, "current_database()") {
			t.Errorf("%s SQL is not scoped to this database", name)
		}
	}
	if !strings.Contains(workloadGroupsSQL, "LIMIT 201") {
		t.Error("groups SQL must read one row past the 200-group cap to detect truncation")
	}
	if !strings.Contains(workloadRolesSQL(testPgss.view), "userid") {
		t.Error("per-role statement time must come from pg_stat_statements.userid")
	}
}

// State: workload_sessions is an extra category on every server version,
// with a reset.
func TestExtraSteps_IncludeWorkloadSessions(t *testing.T) {
	for _, version := range []int{140000, 170000, 180000} {
		c := New(nil, testConfig(), version, noopLog)
		found := false
		for _, st := range c.extraSteps() {
			if st.name != "workload_sessions" {
				continue
			}
			found = true
			snap := &Snapshot{WorkloadSessions: &WorkloadSessions{
				Groups: []WorkloadGroup{{Role: "app"}}}}
			st.reset(snap)
			if snap.WorkloadSessions != nil {
				t.Errorf("reset kept %+v", snap.WorkloadSessions)
			}
		}
		if !found {
			t.Errorf("version %d: no workload_sessions extra step", version)
		}
	}
}

// Error propagation: a failed read marks the category unavailable with
// the cause and clears the partial reading.
func TestWorkloadSessions_FailedReadIsUnknown(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	boom := errors.New("permission denied for view pg_stat_activity")
	c.overrideStep("workload_sessions", func(_ context.Context, s *Snapshot) error {
		s.WorkloadSessions = &WorkloadSessions{Groups: []WorkloadGroup{{Role: "partial"}}}
		return boom
	})
	for _, st := range c.extraSteps() {
		if st.name != "workload_sessions" {
			continue
		}
		snap := &Snapshot{}
		retries := maxCycleRetries
		err := c.runStep(context.Background(), st, snap, &retries)
		if !errors.Is(err, boom) {
			t.Fatalf("runStep error = %v, want the read error", err)
		}
		st.reset(snap)
		if snap.WorkloadSessions != nil {
			t.Fatalf("after reset value=%+v, want nil (unknown)", snap.WorkloadSessions)
		}
		return
	}
	t.Fatal("no workload_sessions extra step")
}

// Tags are parsed from long-running active queries only: an idle in
// transaction session's last statement is not the work it is doing now.
func TestBuildLongSession_TagsOnlyForActive(t *testing.T) {
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	active := buildLongSession(longSessionRow{pid: 7, role: "app", app: "web",
		state: "active", queryStart: start, ageSeconds: 7,
		query: "SELECT pg_sleep(7) /*route='%2Fcheckout',team='pay'*/"})
	if active.Tags["route"] != "/checkout" || active.Tags["team"] != "pay" {
		t.Fatalf("active tags = %v, want route=/checkout team=pay", active.Tags)
	}
	if active.PID != 7 || active.Role != "app" || active.Application != "web" ||
		active.AgeSeconds != 7 || !active.QueryStart.Equal(start) {
		t.Fatalf("active row = %+v", active)
	}
	idle := buildLongSession(longSessionRow{pid: 8, role: "app", state: "idle in transaction",
		ageSeconds: 30, query: "SELECT 1 /*route='x'*/"})
	if len(idle.Tags) != 0 {
		t.Fatalf("idle in transaction tags = %v, want none", idle.Tags)
	}
	if idle.Tags == nil {
		t.Fatal("tags map is nil; want an empty map")
	}
}

// Boundary: 200 groups are kept; a 201st marks the reading truncated.
func TestCapWorkloadGroups(t *testing.T) {
	mk := func(n int) []WorkloadGroup { return make([]WorkloadGroup, n) }
	for _, tc := range []struct {
		n         int
		keep      int
		truncated bool
	}{{0, 0, false}, {200, 200, false}, {201, 200, true}} {
		got, truncated := capWorkloadGroups(mk(tc.n))
		if len(got) != tc.keep || truncated != tc.truncated {
			t.Errorf("%d groups: kept %d truncated %v, want %d %v", tc.n, len(got),
				truncated, tc.keep, tc.truncated)
		}
	}
}

// evid-02b: the age read is bounded, tagged, scoped to this database's
// client sessions and leaves pg_sage's own out, like the group read.
func TestWorkloadAgesSQL_IsBoundedAndExcludesSelf(t *testing.T) {
	sql := workloadAgesSQL
	for _, want := range []string{sageTag, notSelf, "'client backend'", "current_database()",
		"backend_start", "LIMIT "} {
		if !strings.Contains(sql, want) {
			t.Errorf("ages SQL lacks %q", want)
		}
	}
}

// A legacy snapshot decodes with Ages nil (unknown), never an empty list.
func TestWorkloadSessions_LegacyJSONHasNoAges(t *testing.T) {
	var ws WorkloadSessions
	if err := json.Unmarshal([]byte(`{"groups":[],"long":[],"roles":[]}`), &ws); err != nil {
		t.Fatal(err)
	}
	if ws.Ages != nil {
		t.Fatalf("legacy ages %v, want nil", ws.Ages)
	}
}
