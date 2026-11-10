package tuning

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// llm-05: the case packet trims to min(maxPacketBytes,
// llm.context_budget_tokens × 4); a budget of 0 keeps maxPacketBytes.

func bigCasePacket(t *testing.T, budgetBytes int) packet {
	t.Helper()
	s := defaultSettings()
	s.ContextBudgetBytes = budgetBytes
	h := newHarnessWith(t, s)
	var ts []collector.TableStats
	var prevQ, curQ []collector.QueryStats
	long := strings.Repeat("col_with_a_long_name, ", 200)
	for i := int64(1); i <= 40; i++ {
		name := fmt.Sprintf("t%02d", i)
		ts = append(ts, table("public", name, 1000, 0))
		q := fmt.Sprintf("SELECT %s FROM public.%s JOIN public.t01 USING (id)", long, name)
		prevQ = append(prevQ, stmt(i, q, 100, 1000))
		curQ = append(curQ, stmt(i, q, 200, 50000))
	}
	prev := snapAt(t0, prevQ, ts, nil)
	cur := snapAt(t0.Add(time.Minute), curQ, ts, nil)
	w := ClassifyWorkload(cur, nil, t0)
	c := DetectCases(cur, prev, w, DefaultThresholds())[0]
	for _, q := range curQ {
		c.Statements = append(c.Statements, CaseStatement{QueryID: q.QueryID,
			Text: q.Query, Class: ClassApp, Calls: 100, TotalMs: 49000})
	}
	return h.agent.packetFor(context.Background(), c, cur, w, nil, nil)
}

func TestPacketLimit(t *testing.T) {
	for _, tc := range []struct{ budget, want int }{
		{0, maxPacketBytes}, {-1, maxPacketBytes}, {8192, 8192},
		{maxPacketBytes + 1, maxPacketBytes}, {32768, maxPacketBytes},
	} {
		if got := packetLimit(tc.budget); got != tc.want {
			t.Errorf("packetLimit(%d) = %d, want %d", tc.budget, got, tc.want)
		}
	}
}

func TestPacket_TrimsToContextBudget(t *testing.T) {
	small := bigCasePacket(t, 8192)
	if len(small.Text) > 8192 {
		t.Fatalf("packet = %d bytes, budget 8192", len(small.Text))
	}
	if !strings.Contains(small.Text, "truncated") || !strings.Contains(small.Text, "S1") {
		t.Fatal("a budget-cut packet keeps its first evidence and says it was cut")
	}
	full := bigCasePacket(t, 0)
	if len(full.Text) > maxPacketBytes || len(full.Text) <= len(small.Text) {
		t.Fatalf("budget 0: packet = %d bytes, want more than %d and at most %d",
			len(full.Text), len(small.Text), maxPacketBytes)
	}
}

func TestPacket_BudgetBelowInstructionsStillCloses(t *testing.T) {
	pk := bigCasePacket(t, 16)
	if strings.Count(pk.Text, "</data>") != 1 {
		t.Fatalf("a tiny budget must still close the data block:\n%.400s", pk.Text)
	}
	if len(pk.Evidence) != 0 {
		t.Fatalf("evidence = %v, want none in an empty data block", pk.Evidence)
	}
}
