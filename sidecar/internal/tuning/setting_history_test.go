package tuning

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// lifeos (v1.10.0, the old memory advisor): work_mem went 8MB -> 9MB, was
// verified improved, and a new proposal took it to 10MB on cumulative
// pg_stat_database temp totals since the last stats reset: a ratchet.
// Setting and storage-parameter proposals now need evidence measured in
// the interval, wait for the last change's verification, need evidence
// from after that change, and three same-direction changes in 7 days need
// an operator with the history on the card.

func settingAct(id int64, at time.Time, key, from, to, verdict string) SettingAction {
	a := SettingAction{ActionID: id, ExecutedAt: at, Key: key, From: from, To: to,
		Verdict: verdict}
	if verdict != "" && verdict != "pending" {
		d := at.Add(30 * time.Minute)
		a.DecidedAt = &d
	}
	return a
}

func spillCase(windowed bool, temp int64) Case {
	c := ordersCase()
	c.Statements[0].Windowed, c.Statements[0].TempBlksWritten = windowed, temp
	return c
}

func TestJudge_WorkMemOnCumulativeCountersIsNoEvidence(t *testing.T) {
	h := newHarness(t)
	j := judgeCase(t, h, nil, spillCase(false, 12_407_021), gucProposal("work_mem", "64MB",
		-60))
	if j.Verdict != VerdictRejected || j.Reason != ReasonNoRecentEvidence {
		t.Fatalf("cumulative-since-reset spills are not evidence: %+v", j)
	}
	j = judgeCase(t, h, nil, spillCase(true, 0), gucProposal("work_mem", "64MB", -60))
	if j.Verdict != VerdictRejected || j.Reason != ReasonNoRecentEvidence {
		t.Fatalf("no spill in the interval, no work_mem change: %+v", j)
	}
}

func TestJudge_WorkMemCitesItsWindow(t *testing.T) {
	h := newHarness(t)
	j := judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60))
	if j.Verdict != VerdictAdmitted {
		t.Fatalf("spills in the interval: %+v", j)
	}
	w, _ := j.Finding.Detail["evidence_window"].(map[string]any)
	if w["from"] != t0.UTC().Format(time.RFC3339) ||
		w["to"] != t0.Add(5*time.Minute).UTC().Format(time.RFC3339) ||
		j.Finding.Detail["temp_blks_in_window"] != int64(4800) {
		t.Fatalf("detail = %v", j.Finding.Detail)
	}
}

func TestJudge_SettingWaitsForItsVerification(t *testing.T) {
	for _, verdict := range []string{"", "pending"} {
		h := newHarness(t)
		h.store.actions = []SettingAction{settingAct(6407, t0.Add(-time.Hour), "work_mem",
			"8MB", "9MB", verdict)}
		j := judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60))
		if j.Verdict != VerdictRejected || j.Reason != ReasonVerificationPending ||
			!strings.Contains(j.Detail, "6407") {
			t.Fatalf("verdict %q: %+v", verdict, j)
		}
	}
}

func TestJudge_StorageParameterWaitsForItsVerification(t *testing.T) {
	h := newHarness(t)
	h.store.actions = []SettingAction{settingAct(7, t0.Add(-time.Hour),
		"public.orders|autovacuum_vacuum_scale_factor", "0.2", "0.1", "pending")}
	j := judgeOne(t, h, nil, relProposal("autovacuum_vacuum_scale_factor", "0.02", -40))
	if j.Verdict != VerdictRejected || j.Reason != ReasonVerificationPending {
		t.Fatalf("judged = %+v", j)
	}
	if j := judgeOne(t, h, nil, relProposal("fillfactor", "90", 10)); j.Verdict !=
		VerdictAdmitted {
		t.Fatalf("another parameter of the table is not blocked: %+v", j)
	}
}

func TestJudge_AfterImprovedNeedsEvidenceFromAfterTheChange(t *testing.T) {
	h := newHarness(t)
	// Changed inside the window (t0+1m): the window mixes before and after.
	h.store.actions = []SettingAction{settingAct(6407, t0.Add(time.Minute), "work_mem",
		"8MB", "9MB", "improved")}
	j := judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60))
	if j.Verdict != VerdictRejected || j.Reason != ReasonNoRecentEvidence {
		t.Fatalf("evidence from before the change: %+v", j)
	}
	// Changed before the window, and no spill after it: nothing to fix.
	h.store.actions = []SettingAction{settingAct(6407, t0.Add(-time.Hour), "work_mem",
		"8MB", "9MB", "improved")}
	j = judgeCase(t, h, nil, spillCase(true, 0), gucProposal("work_mem", "64MB", -60))
	if j.Verdict != VerdictRejected || j.Reason != ReasonNoRecentEvidence {
		t.Fatalf("improved and no spill since: %+v", j)
	}
	// Spills measured after the change: a further change may be proposed.
	if j = judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60)); j.Verdict !=
		VerdictAdmitted {
		t.Fatalf("new evidence after the change: %+v", j)
	}
}

func TestJudge_RatchetGuardBoundary(t *testing.T) {
	day := 24 * time.Hour
	up1 := settingAct(1, t0.Add(-3*day), "work_mem", "4MB", "8MB", "improved")
	up2 := settingAct(2, t0.Add(-2*day), "work_mem", "8MB", "9MB", "improved")
	down := settingAct(3, t0.Add(-2*day), "work_mem", "9MB", "4MB", "regressed")
	old := settingAct(4, t0.Add(-8*day), "work_mem", "2MB", "3MB", "improved")
	for _, tc := range []struct {
		name    string
		acts    []SettingAction
		gated   bool
		history string
	}{
		{"one earlier increase: this is the second", []SettingAction{up1}, false, ""},
		{"two earlier increases: this is the third", []SettingAction{up1, up2}, true,
			"8MB -> 9MB"},
		{"a decrease does not count", []SettingAction{up1, down}, false, ""},
		{"older than 7 days does not count", []SettingAction{old, up1}, false, ""},
	} {
		h := newHarness(t)
		h.store.actions = tc.acts
		j := judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60))
		if j.Verdict != VerdictAdmitted {
			t.Fatalf("%s: %+v", tc.name, j)
		}
		why, gated := j.Finding.Detail[analyzer.DetailApprovalRequired].(string)
		if gated != tc.gated || (tc.gated && (!strings.Contains(why, tc.history) ||
			!strings.Contains(why, "3 times in 7 days"))) {
			t.Fatalf("%s: approval %q (gated %v)", tc.name, why, gated)
		}
		if tc.gated && j.Finding.Detail["setting_history"] == nil {
			t.Fatalf("%s: the history is on the card: %v", tc.name, j.Finding.Detail)
		}
	}
}

func TestJudge_SettingHistoryUnreadableRefuses(t *testing.T) {
	h := newHarness(t)
	h.store.actErr = errFake
	j := judgeOne(t, h, nil, gucProposal("work_mem", "64MB", -60))
	if j.Verdict != VerdictRejected || j.Reason != ReasonUnavailable {
		t.Fatalf("without the history the ratchet guard cannot hold: %+v", j)
	}
	if j := judgeOne(t, h, nil, createProposal()); j.Verdict != VerdictAdmitted {
		t.Fatalf("index proposals do not need the setting history: %+v", j)
	}
}

func TestSettingActionsFromLedger(t *testing.T) {
	at := t0.Add(-time.Hour)
	rows := []ledgerAction{
		{ID: 1, ExecutedAt: at, SQL: "ALTER SYSTEM SET work_mem = '9MB'",
			Rollback: "ALTER SYSTEM SET work_mem = '8MB'", Outcome: "success",
			Verdict: "improved"},
		{ID: 2, ExecutedAt: at, SQL: "ALTER SYSTEM SET work_mem TO '16MB'",
			Rollback: "ALTER SYSTEM RESET work_mem", Outcome: "success"},
		{ID: 3, ExecutedAt: at, SQL: "ALTER TABLE public.orders SET (fillfactor = 90, " +
			"autovacuum_vacuum_scale_factor = 0.05)", Rollback: "ALTER TABLE public.orders " +
			"RESET (fillfactor, autovacuum_vacuum_scale_factor)", Outcome: "pending"},
		{ID: 4, ExecutedAt: at, SQL: "CREATE INDEX CONCURRENTLY x ON t (a)",
			Outcome: "success"},
		{ID: 5, ExecutedAt: at, SQL: "ALTER SYSTEM SET work_mem = '32MB'",
			Outcome: "failed"},
	}
	got := settingActionsFrom(rows)
	if len(got) != 4 {
		t.Fatalf("actions = %+v", got)
	}
	if got[0].Key != "work_mem" || got[0].From != "8MB" || got[0].To != "9MB" ||
		got[0].Verdict != "improved" || got[1].From != "" || got[1].To != "16MB" {
		t.Fatalf("system settings = %+v %+v", got[0], got[1])
	}
	if got[2].Key != "public.orders|fillfactor" || got[2].To != "90" ||
		got[3].Key != "public.orders|autovacuum_vacuum_scale_factor" ||
		got[2].Verdict != "pending" {
		t.Fatalf("storage parameters = %+v %+v", got[2], got[3])
	}
}

func TestTools_StatementShowsNoCumulativeCounters(t *testing.T) {
	h := newHarness(t)
	out := call(t, ordersToolbox(t, h), "statement", `{"queryid":101}`)
	if _, ok := out["cumulative"]; ok {
		t.Fatalf("counters since the last stats reset are not current evidence: %v", out)
	}
}

func TestPacket_LabelsStatementsWithoutAnInterval(t *testing.T) {
	h := newHarness(t)
	_, cur := ordersPair()
	w := ClassifyWorkload(cur, nil, t0)
	cs := DetectCases(cur, nil, w, DefaultThresholds())
	if len(cs) == 0 || cs[0].Statements[0].Windowed {
		t.Fatalf("without a previous snapshot nothing is windowed: %+v", cs)
	}
	pk := h.agent.packetFor(context.Background(), cs[0], cur, w, nil, nil)
	if !strings.Contains(pk.Text, "no earlier sample") {
		t.Fatalf("cumulative counters are labeled as such:\n%s", pk.Text)
	}
	prev, cur := ordersPair()
	cs = DetectCases(cur, prev, ClassifyWorkload(cur, nil, t0), DefaultThresholds())
	if !cs[0].Statements[0].Windowed {
		t.Fatalf("an interval statement is windowed: %+v", cs[0].Statements[0])
	}
}

func TestTune_PendingVerificationBlocksEndToEnd(t *testing.T) {
	for _, pending := range []bool{false, true} {
		h := newHarness(t, answer(proposalsJSON(t,
			map[string]any{"type": "guc", "name": "work_mem", "value": "64MB",
				"evidence": []string{"S1"}, "expected_change_pct": -60})))
		if pending {
			h.store.actions = []SettingAction{settingAct(6407, t0.Add(-time.Hour),
				"work_mem", "8MB", "9MB", "pending")}
		}
		prev, cur := ordersPair()
		prev.Queries[0].TempBlksWritten, cur.Queries[0].TempBlksWritten = 100, 5000
		cur.ConfigData = validationSnap().ConfigData
		out, err := h.agent.Tune(context.Background(), cur, prev)
		if err != nil {
			t.Fatalf("tune: %v", err)
		}
		if _, ok := findingByCategory(out.Findings, "memory_tuning"); ok == pending {
			t.Fatalf("pending %v: findings %+v", pending, out.Findings)
		}
	}
}
