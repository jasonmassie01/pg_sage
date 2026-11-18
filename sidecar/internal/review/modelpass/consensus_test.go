package modelpass

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// Consensus boundaries: one run, 2 of 3 and 1 of 3 keeps, hypotheses by
// normalized (kind, target), refutation and the raw output cap
// (review-03a).

func TestReview_OneRunAppliesNoConsensus(t *testing.T) {
	in := input(t, "db1")
	m := always(keepRanked(t, in, "t_c", "t_a"))
	rep, err := Review(context.Background(), settings(1, "majority"), deps(m, nil), in)
	if err != nil || rep.Consensus != report.ConsensusSingle || ids(rep.Ranked) != "t_c,t_a" ||
		rep.Runs != 1 || m.count() != 1 {
		t.Fatalf("single run: %q %s runs %d calls %d (%v)", rep.Consensus, ids(rep.Ranked),
			rep.Runs, m.count(), err)
	}
}

func TestReview_TwoOfThreeKeepsAndOneOfThreeDrops(t *testing.T) {
	in := input(t, "db1")
	// t_a: 3 of 3; t_b: 2 of 3; t_c: 1 of 3; t_d: 0.
	plans := [][]string{{"t_a", "t_b", "t_c"}, {"t_a", "t_b"}, {"t_a"}}
	m := &scripted{reply: func(n int, _ []llm.Message) (llm.ToolResult, error) {
		return native(keepRanked(t, in, plans[n%3]...)), nil
	}}
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
	if err != nil || ids(rep.Ranked) != "t_a,t_b" {
		t.Fatalf("ranked %s (%v), want t_a,t_b", ids(rep.Ranked), err)
	}
	if rep.Ranked[0].Votes != 3 || rep.Ranked[1].Votes != 2 {
		t.Fatalf("votes = %d, %d, want 3 and 2", rep.Ranked[0].Votes, rep.Ranked[1].Votes)
	}
	votes := map[string]int{}
	for _, d := range rep.Dropped {
		votes[d.CandidateID] = d.Votes
	}
	if v, ok := votes[dropOf("t_c").ID]; !ok || v != 1 {
		t.Fatalf("dropped = %+v, want t_c with 1 vote", rep.Dropped)
	}
}

// needs_evidence and drop are not keeps.
func TestReview_NeedsEvidenceIsNotAKeep(t *testing.T) {
	in := input(t, "db1")
	m := &scripted{reply: func(n int, _ []llm.Message) (llm.ToolResult, error) {
		a := keepRanked(t, in, "t_a")
		if n%3 != 0 {
			a.Ranking[0].Decision = report.DecisionNeedsEvidence
		}
		if n%3 == 2 {
			a.Ranking[0].Decision = report.DecisionDrop
		}
		return native(a), nil
	}}
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
	if err != nil || len(rep.Ranked) != 0 || rep.ModelPass != report.PassDone {
		t.Fatalf("ranked %s, pass %s (%v), want nothing kept by a done pass",
			ids(rep.Ranked), rep.ModelPass, err)
	}
}

func TestReview_InvalidDecisionAndRankAreRejected(t *testing.T) {
	in := input(t, "db1")
	a := keepRanked(t, in, "t_a", "t_b", "t_c")
	a.Ranking[1].Decision = "ship it"
	a.Ranking[2].Rank = 0
	a.Ranking = append(a.Ranking, a.Ranking[0])
	rep, err := Review(context.Background(), settings(1, "majority"), deps(always(a), nil), in)
	if err != nil || ids(rep.Ranked) != "t_a" {
		t.Fatalf("ranked %s (%v), want only t_a", ids(rep.Ranked), err)
	}
	for _, want := range []string{"invalid decision", "invalid rank", "duplicate candidate"} {
		if !hasRejection(rep, want) {
			t.Errorf("rejected = %+v, want %q", rep.Rejected, want)
		}
	}
}

func TestReview_HypothesesSurviveByNormalizedKindAndTarget(t *testing.T) {
	in := input(t, "db1")
	fact := aliasOf(t, in, "index/idx/public.t_a")
	hyp := func(kind, target string) hypothesis {
		return hypothesis{Title: "look at " + target, Kind: kind, Target: target,
			Claims: []agentloop.Claim{claim("public.t_a had 0 scans", fact)}}
	}
	m := &scripted{reply: func(n int, _ []llm.Message) (llm.ToolResult, error) {
		a := keepRanked(t, in, "t_a")
		switch n % 3 {
		case 0:
			a.Hypotheses = []hypothesis{hyp("Vacuum", "public.t"), hyp("stats", "public.u")}
		case 1:
			a.Hypotheses = []hypothesis{hyp(" vacuum ", "PUBLIC.T")}
		case 2:
			a.Hypotheses = []hypothesis{{Title: "uncited", Kind: "x", Target: "y"}}
		}
		return native(a), nil
	}}
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
	if err != nil || len(rep.Hypotheses) != 1 || rep.Hypotheses[0].Votes != 2 ||
		!strings.EqualFold(strings.TrimSpace(rep.Hypotheses[0].Kind), "vacuum") {
		t.Fatalf("hypotheses = %+v (%v), want the vacuum one with 2 votes", rep.Hypotheses, err)
	}
	if !hasRejection(rep, "uncited hypothesis") {
		t.Fatalf("rejected = %+v, want the uncited hypothesis", rep.Rejected)
	}
}

func TestReview_LinksAndReportTextNeedCitations(t *testing.T) {
	in := input(t, "db1")
	fact := aliasOf(t, in, "index/idx/public.t_a")
	a := keepRanked(t, in, "t_a", "t_b")
	a.Links = []linkItem{
		{A: dropOf("t_a").ID, B: dropOf("t_b").ID, Relation: "related",
			Claims: []agentloop.Claim{claim("public.t_a had 0 scans", fact)}},
		{A: dropOf("t_a").ID, B: dropOf("t_b").ID, Relation: "conflicts"},
		{A: dropOf("t_a").ID, B: "nope", Relation: "related",
			Claims: []agentloop.Claim{claim("public.t_a had 0 scans", fact)}},
		{A: dropOf("t_a").ID, B: dropOf("t_c").ID, Relation: "loves",
			Claims: []agentloop.Claim{claim("public.t_a had 0 scans", fact)}}}
	a.Report = []reportItem{{Dimension: "indexes", Text: "Two indexes had 0 scans.",
		Claims: []agentloop.Claim{claim("public.t_a had 0 scans", fact)}},
		{Dimension: "vacuum", Text: "All good."}}
	rep, err := Review(context.Background(), settings(1, "majority"), deps(always(a), nil), in)
	if err != nil || len(rep.Links) != 1 || rep.Links[0].Relation != "related" {
		t.Fatalf("links = %+v (%v), want the cited related link", rep.Links, err)
	}
	for _, want := range []string{"uncited link", "unknown candidate", "invalid relation",
		"uncited report text"} {
		if !hasRejection(rep, want) {
			t.Errorf("rejected = %+v, want %q", rep.Rejected, want)
		}
	}
	if rep.Sections[0].Origin != report.OriginModel ||
		rep.Sections[0].Text != "Two indexes had 0 scans." {
		t.Fatalf("section = %+v, want the cited model text", rep.Sections[0])
	}
}

func TestReview_RefuteDropsWhatACitedRefutationAttacks(t *testing.T) {
	in := input(t, "db1")
	factB := aliasOf(t, in, "index/idx/public.t_b")
	m := &scripted{reply: func(n int, _ []llm.Message) (llm.ToolResult, error) {
		if n == 0 {
			return native(keepRanked(t, in, "t_a", "t_b", "t_c")), nil
		}
		r := refutations{Refutations: []refutation{
			{CandidateID: dropOf("t_b").ID, Argument: "it is read by a report",
				Claims: []agentloop.Claim{claim("public.t_b is 16384 bytes", factB)}},
			{CandidateID: dropOf("t_c").ID, Argument: "made up",
				Claims: []agentloop.Claim{claim("public.t_c is read", "E999")}}}}
		res := native(r)
		res.ToolCalls[0].Name = refuteName
		return res, nil
	}}
	store := &memStore{}
	rep, err := Review(context.Background(), settings(3, "refute"), deps(m, store), in)
	if err != nil || rep.Consensus != report.ConsensusRefute || ids(rep.Ranked) != "t_a,t_c" {
		t.Fatalf("refute: %q %s (%v), want t_a,t_c", rep.Consensus, ids(rep.Ranked), err)
	}
	var refuted bool
	for _, d := range rep.Dropped {
		refuted = refuted || (d.CandidateID == dropOf("t_b").ID &&
			strings.Contains(d.Reason, "refuted"))
	}
	if !refuted || m.count() != 2 {
		t.Fatalf("dropped = %+v after %d calls, want t_b refuted by run 2", rep.Dropped, m.count())
	}
	runs := store.all()
	if len(runs) != 2 || runs[1].Role != RoleRefute || runs[1].Attempt != 2 {
		t.Fatalf("stored runs = %+v, want rank then refute", runs)
	}
	if !strings.Contains(m.calls[1].msgs[1].Content, dropOf("t_a").ID) {
		t.Fatal("the refuter was not shown the kept items")
	}
}

func TestReview_RefuteRunFailureFallsBack(t *testing.T) {
	in := input(t, "db1")
	m := &scripted{reply: func(n int, _ []llm.Message) (llm.ToolResult, error) {
		if n == 0 {
			return native(keepRanked(t, in, "t_a")), nil
		}
		return llm.ToolResult{}, llm.ErrEmptyResponse
	}}
	rep, err := Review(context.Background(), settings(3, "refute"), deps(m, nil), in)
	if err != nil || rep.ModelPass != report.PassFailed ||
		!strings.Contains(rep.ModelPassReason, "refutation") {
		t.Fatalf("pass %s %q (%v), want failed on the refutation run", rep.ModelPass,
			rep.ModelPassReason, err)
	}
	checkFallback(t, rep, in)
}

func TestCapRaw_TruncatesAt256KiB(t *testing.T) {
	exact := strings.Repeat("x", MaxRawOutputBytes)
	if got, cut := capRaw(exact); cut || len(got) != MaxRawOutputBytes {
		t.Fatalf("256 KiB: %d bytes, cut %v, want kept whole", len(got), cut)
	}
	if got, cut := capRaw(exact + "y"); !cut || len(got) != MaxRawOutputBytes ||
		strings.Contains(got, "y") {
		t.Fatalf("256 KiB + 1: %d bytes, cut %v, want truncated with the flag", len(got), cut)
	}
	multi := strings.Repeat("x", MaxRawOutputBytes-1) + "é"
	if got, cut := capRaw(multi); !cut || len(got) != MaxRawOutputBytes-1 {
		t.Fatalf("split rune: %d bytes, cut %v, want the rune left out whole", len(got), cut)
	}
	if got, cut := capRaw(""); cut || got != "" {
		t.Fatal("empty output was cut")
	}
}

func TestReview_LongOutputIsStoredTruncatedWithItsFullHash(t *testing.T) {
	in := input(t, "db1")
	a := keepRanked(t, in, "t_a")
	a.Ranking[0].Attack = strings.Repeat("z", MaxRawOutputBytes)
	store := &memStore{}
	if _, err := Review(context.Background(), settings(1, "majority"),
		deps(always(a), store), in); err != nil {
		t.Fatalf("Review: %v", err)
	}
	runs := store.all()
	if len(runs) != 1 || !runs[0].Truncated || len(runs[0].RawOutput) != MaxRawOutputBytes ||
		runs[0].OutputSHA256 == "" {
		t.Fatalf("run = truncated %v, %d bytes", runs[0].Truncated, len(runs[0].RawOutput))
	}
}
