package modelpass

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// Review additions (review-03a): prose grounding and the median rank,
// which no earlier test pinned.

// Prose (attack, rationale, report text) holding a number its kept
// claims do not ground is dropped; grounded prose stays.
func TestReview_UngroundedProseIsDropped(t *testing.T) {
	in := input(t, "db1")
	fact := aliasOf(t, in, "index/idx/public.t_a")
	a := keepRanked(t, in, "t_a", "t_b")
	a.Ranking[1].Attack = "it saves 4242 MB"
	a.Hypotheses = []hypothesis{{Title: "h", Kind: "advice", Target: "public.t_a",
		Rationale: "costs 777 ms", Claims: []agentloop.Claim{claim("t_a had 0 scans", fact)}}}
	a.Report = []reportItem{{Dimension: "indexes", Text: "31337 indexes are unused.",
		Claims: []agentloop.Claim{claim("public.t_a had 0 scans", fact)}}}
	rep, err := Review(context.Background(), settings(1, "majority"), deps(always(a), nil), in)
	if err != nil || rep.ModelPass != report.PassDone || ids(rep.Ranked) != "t_a,t_b" {
		t.Fatalf("Review: %s %q %s (%v)", rep.ModelPass, rep.ModelPassReason,
			ids(rep.Ranked), err)
	}
	if rep.Ranked[0].Attack != "it has 0 scans" || rep.Ranked[1].Attack != "" {
		t.Fatalf("attacks %q, %q; want the grounded one kept and 4242 dropped",
			rep.Ranked[0].Attack, rep.Ranked[1].Attack)
	}
	if len(rep.Hypotheses) != 1 || rep.Hypotheses[0].Rationale != "" {
		t.Fatalf("hypotheses = %+v, want the 777 rationale dropped", rep.Hypotheses)
	}
	if rep.Sections[0].Origin != report.OriginDeterministic || !hasRejection(rep,
		"ungrounded report text") {
		t.Fatalf("section %+v, rejected %+v; want the 31337 text refused", rep.Sections[0],
			rep.Rejected)
	}
	for _, n := range []string{"4242", "777", "31337"} {
		if !hasDroppedClaim(rep, n, agentloop.DropUngrounded) {
			t.Errorf("dropped claims = %+v, want %s ungrounded", rep.DroppedClaims, n)
		}
	}
}

// The rank is the median of the keeping runs' ranks, not the worst or
// the first: t_a ranks 1, 1, 3 and t_b 2, 2, 1.
func TestReview_RankIsTheMedianOfTheRuns(t *testing.T) {
	in := input(t, "db1")
	orders := [][]string{{"t_a", "t_b"}, {"t_a", "t_b"}, {"t_b", "t_c", "t_a"}}
	m := &scripted{reply: func(n int, _ []llm.Message) (llm.ToolResult, error) {
		return native(keepRanked(t, in, orders[n%3]...)), nil
	}}
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
	if err != nil || rep.ModelPass != report.PassDone {
		t.Fatalf("Review: %s %q (%v)", rep.ModelPass, rep.ModelPassReason, err)
	}
	if got := ids(rep.Ranked); got != "t_a,t_b" {
		t.Fatalf("ranked %s, want t_a (median 1) before t_b (median 2); t_c has 1 vote", got)
	}
}
