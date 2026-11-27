package approvalcard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/verify"
)

// lab-03a: the card shows what the change was tested on before
// production (the queue row's ladder), and says when nothing was.

var ladderAt = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func ladderDrop(id int) store.QueuedAction {
	return queued(id, "drop_unused_index", `DROP INDEX CONCURRENTLY "public"."idx_old"`,
		"CREATE INDEX CONCURRENTLY idx_old ON public.t (a)", "moderate")
}

func ladderInputs(a store.QueuedAction, raw string) Inputs {
	in := Inputs{Database: "orders", Action: a, Now: now, TrustLevel: "autonomous",
		Finding: &FindingRow{ID: a.FindingID, Category: "unused_index",
			Severity: "info", ObjectType: "index", Object: "public.idx_old",
			Title: "Unused index public.idx_old", Detail: map[string]any{}}}
	if raw != "" {
		in.Ladder = []byte(raw)
	}
	return in
}

func ladderJSON(t *testing.T, recs ...verify.LadderRecord) string {
	t.Helper()
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func tierOne() verify.LadderRecord {
	pct := -41.0
	return verify.FromDropMeasurement(verify.WhatIfEvidence{Verdict: "verified",
		ChangePct: &pct, Measured: 4}, ladderAt)
}

func cloneSkipped() verify.LadderRecord {
	return verify.LadderRecord{Tier: verify.TierClone, Name: "clone",
		Result: verify.LadderSkipped, At: ladderAt, Skipped: []verify.SkippedTier{
			{Tier: verify.TierClone, Reason: "no clone provider configured"}}}
}

func hasReason(c Card, code string) bool {
	for _, r := range c.Why {
		if r.Code == code {
			return true
		}
	}
	return false
}

func TestLadder_CardCarriesTheRecordsInOrder(t *testing.T) {
	a := ladderDrop(61)
	c := Assemble(ladderInputs(a, ladderJSON(t, tierOne(), cloneSkipped())))
	if len(c.Ladder) != 2 || c.Ladder[0].Tier != verify.TierPlanOnly ||
		c.Ladder[1].Tier != verify.TierClone {
		t.Fatalf("ladder = %+v, want tier 1 then tier 2", c.Ladder)
	}
	if c.Ladder[0].Predicted == nil || *c.Ladder[0].Predicted != -41 {
		t.Fatalf("tier 1 predicted = %v", c.Ladder[0].Predicted)
	}
	if c.LadderNote != "" {
		t.Fatalf("note = %q, want none when records exist", c.LadderNote)
	}
}

// Nil/empty: a row from before the migration (NULL) says so.
func TestLadder_NullShowsNoPreProductionTest(t *testing.T) {
	for _, raw := range []string{"", "null", "[]"} {
		c := Assemble(ladderInputs(ladderDrop(62), raw))
		if c.Ladder != nil || c.LadderNote != LadderNoneRecorded {
			t.Errorf("%q: ladder %+v note %q, want the none-recorded note", raw, c.Ladder,
				c.LadderNote)
		}
	}
	if LadderNoneRecorded != "No pre-production test recorded" {
		t.Fatalf("note text = %q", LadderNoneRecorded)
	}
}

// Invalid input: a stored ladder that is not an array gives no section,
// no note and one WARN naming the queue id; the card still renders.
func TestLadder_NotAnArrayGivesNoSectionAndOneWarn(t *testing.T) {
	for _, raw := range []string{`{"tier":1}`, `"passed"`, `[{"tier":"x"}]`} {
		buf := captureWarnings(t)
		c := Assemble(ladderInputs(ladderDrop(63), raw))
		if c.Ladder != nil || c.LadderNote != "" {
			t.Errorf("%s: ladder %+v note %q, want neither", raw, c.Ladder, c.LadderNote)
		}
		if c.QueueID != 63 || c.SQL == "" {
			t.Errorf("%s: card did not render: %+v", raw, c)
		}
		var warns []string
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "ladder") {
				warns = append(warns, line)
			}
		}
		if len(warns) != 1 || !strings.Contains(warns[0], "queue_id=63") {
			t.Errorf("%s: warnings = %q, want one naming queue_id=63", raw, warns)
		}
	}
}

func TestLadder_CostlyWithoutCloneSaysNotRehearsed(t *testing.T) {
	c := Assemble(ladderInputs(ladderDrop(64), ladderJSON(t, tierOne(), cloneSkipped())))
	if !hasReason(c, "not_rehearsed") {
		t.Fatalf("why = %+v, want not_rehearsed", c.Why)
	}
	for _, r := range c.Why {
		if r.Code == "not_rehearsed" && !strings.Contains(r.Text, "not rehearsed on a clone") {
			t.Fatalf("reason text = %q", r.Text)
		}
	}
	clone := verify.FromRehearsal(verify.Rehearsal{Passed: true, ReceiptID: 812}, ladderAt)
	c = Assemble(ladderInputs(ladderDrop(65), ladderJSON(t, tierOne(), clone)))
	if hasReason(c, "not_rehearsed") {
		t.Fatalf("a rehearsed drop says not rehearsed: %+v", c.Why)
	}
	// A cheap class never says it.
	idx := queued(66, "create_index_concurrently",
		"CREATE INDEX CONCURRENTLY i ON public.t (a)", "DROP INDEX CONCURRENTLY i", "safe")
	if hasReason(Assemble(ladderInputs(idx, "")), "not_rehearsed") {
		t.Fatal("an index create says not rehearsed")
	}
}

// Content hash (golden): adding a ladder changes nothing a decision binds.
func TestContentHash_UnchangedByLadder(t *testing.T) {
	a := ladderDrop(67)
	without := Assemble(ladderInputs(a, ""))
	with := Assemble(ladderInputs(a, ladderJSON(t, tierOne(), cloneSkipped())))
	if len(with.Ladder) == 0 {
		t.Fatal("premise: the card has no ladder")
	}
	if with.CardHash != without.CardHash || with.CardHash != ContentHash(a) {
		t.Fatalf("card hash with %s, without %s, content %s", with.CardHash,
			without.CardHash, ContentHash(a))
	}
}
