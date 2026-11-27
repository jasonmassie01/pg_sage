package modelpass

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/review/report"
)

// The model pass's happy path, invalid model output and empty input
// (review-03a).

func TestReview_KeepsWhatEveryRunKeepsInMedianRankOrder(t *testing.T) {
	in := input(t, "db1")
	orders := [][]string{{"t_a", "t_b", "t_c"}, {"t_b", "t_a", "t_c"}, {"t_a", "t_c", "t_b"}}
	m := &scripted{reply: func(n int, _ []llm.Message) (llm.ToolResult, error) {
		return native(keepRanked(t, in, orders[n%3]...)), nil
	}}
	store := &memStore{}
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, store), in)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if rep.ModelPass != report.PassDone || rep.Consensus != report.ConsensusMajority ||
		rep.Runs != 3 || rep.RunsDone != 3 {
		t.Fatalf("pass = %s %q runs %d/%d", rep.ModelPass, rep.Consensus, rep.RunsDone, rep.Runs)
	}
	if got := ids(rep.Ranked); got != "t_a,t_b,t_c" {
		t.Fatalf("ranked %s, want t_a,t_b,t_c by median rank", got)
	}
	for i, r := range rep.Ranked {
		fact := "index/idx/public." + indexNames[i]
		if r.Rank != i+1 || r.Votes != 3 || r.Origin != report.OriginModel ||
			len(r.Claims) != 1 || len(r.Claims[0].EvidenceIDs) != 1 ||
			r.Claims[0].EvidenceIDs[0] != fact || r.EvidenceIDs[0] != fact {
			t.Errorf("ranked %d = %+v, want rank %d, 3 votes, the claim on %s", i, r, i+1, fact)
		}
	}
	if len(rep.Dropped) != 1 || rep.Dropped[0].CandidateID != dropOf("t_d").ID ||
		rep.Dropped[0].Votes != 0 || !strings.Contains(rep.Dropped[0].Reason, "0 of 3") {
		t.Fatalf("dropped = %+v, want t_d kept by 0 of 3 runs", rep.Dropped)
	}
	checkStoredRuns(t, store.all(), in, 3)
	if m.count() != 3 {
		t.Fatalf("model calls = %d, want one per run", m.count())
	}
	for _, c := range m.calls {
		if llm.PurposeOf(c.ctx) != llm.PurposeReview {
			t.Fatalf("a call's purpose = %q, want review", llm.PurposeOf(c.ctx))
		}
	}
}

func checkStoredRuns(t *testing.T, runs []RunRecord, in Input, n int) {
	t.Helper()
	if len(runs) != n {
		t.Fatalf("stored %d runs, want %d", len(runs), n)
	}
	seen := map[int]bool{}
	for _, r := range runs {
		sum := sha256.Sum256([]byte(r.RawOutput))
		if r.RunID != in.RunID || r.Status != StatusDone || r.PackHash != in.Pack.ContentSHA256 ||
			r.OutputSHA256 != hex.EncodeToString(sum[:]) || r.Truncated ||
			r.PromptVersion != PromptVersion || r.Model != "test-model" || r.Role != RoleRank ||
			r.TokensIn != 1000 || r.TokensOut != 200 || r.CostUSD != nil || r.KeptClaims != 3 ||
			!strings.Contains(r.RawOutput, finalName) {
			t.Errorf("run = %+v", r)
		}
		seen[r.Attempt] = true
	}
	for a := 1; a <= n; a++ {
		if !seen[a] {
			t.Errorf("attempt %d not stored", a)
		}
	}
}

func TestReview_MarkdownFencedJSONIsParsed(t *testing.T) {
	in := input(t, "db1")
	a := keepRanked(t, in, "t_b")
	body := native(map[string]any{"tool": finalName, "args": a}).ToolCalls[0].Arguments
	m := &scripted{reply: func(int, []llm.Message) (llm.ToolResult, error) {
		return llm.ToolResult{Content: "Here it is:\n```json\n" + string(body) + "\n```"}, nil
	}}
	d := deps(m, nil)
	d.Protocol = agentloop.ProtocolJSON
	rep, err := Review(context.Background(), settings(1, "majority"), d, in)
	if err != nil || rep.ModelPass != report.PassDone || ids(rep.Ranked) != "t_b" {
		t.Fatalf("fenced JSON: %s %q %s (%v)", rep.ModelPass, rep.ModelPassReason,
			ids(rep.Ranked), err)
	}
}

func TestReview_MalformedJSONFailsTheRunAfterTheRetryLimit(t *testing.T) {
	in := input(t, "db1")
	m := &scripted{reply: func(int, []llm.Message) (llm.ToolResult, error) {
		return llm.ToolResult{Content: `{"tool": "submit_review", "args": {`}, nil
	}}
	d := deps(m, &memStore{})
	d.Protocol, d.Budget.MaxSteps = agentloop.ProtocolJSON, 10
	rep, err := Review(context.Background(), settings(1, "majority"), d, in)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if rep.ModelPass != report.PassFailed || m.count() != agentloop.MaxBadReplies ||
		!strings.Contains(rep.ModelPassReason, agentloop.StopMalformed) {
		t.Fatalf("pass %s %q after %d calls, want failed on malformed replies after %d",
			rep.ModelPass, rep.ModelPassReason, m.count(), agentloop.MaxBadReplies)
	}
	checkFallback(t, rep, in)
}

// checkFallback: the deterministic ranking of every candidate and the
// templated text of every graded dimension.
func checkFallback(t *testing.T, rep report.Report, in Input) {
	t.Helper()
	if rep.Consensus != report.ConsensusDeterministic || len(rep.Ranked) != len(in.Candidates) {
		t.Fatalf("fallback = %q with %d ranked, want deterministic with %d", rep.Consensus,
			len(rep.Ranked), len(in.Candidates))
	}
	for _, r := range rep.Ranked {
		if r.Origin != report.OriginDeterministic || len(r.Claims) != 0 {
			t.Errorf("fallback entry %+v carries model output", r)
		}
	}
	if len(rep.Sections) == 0 || rep.Sections[0].Origin != report.OriginDeterministic ||
		!strings.HasPrefix(rep.Sections[0].Text, "Indexes: grade C") {
		t.Fatalf("sections = %+v, want the templated grade text", rep.Sections)
	}
	if rep.Overall != "C" || rep.RubricVersion != "pg_sage-rubric/1" {
		t.Fatalf("report lost its grade: %q %q", rep.Overall, rep.RubricVersion)
	}
}

func TestReview_UnknownCandidateAndUnknownEvidenceAreDropped(t *testing.T) {
	in := input(t, "db1")
	a := keepRanked(t, in, "t_a")
	a.Ranking = append(a.Ranking, rankItem{CandidateID: strings.Repeat("f", 64), Rank: 2,
		Decision: report.DecisionKeep, Claims: []agentloop.Claim{claim("made up", "E1")}})
	a.Ranking[0].Claims = append(a.Ranking[0].Claims, claim("it is huge", "E999"))
	rep, err := Review(context.Background(), settings(1, "majority"), deps(always(a), nil), in)
	if err != nil || rep.ModelPass != report.PassDone {
		t.Fatalf("Review: %s %q %v", rep.ModelPass, rep.ModelPassReason, err)
	}
	if ids(rep.Ranked) != "t_a" || len(rep.Ranked[0].Claims) != 1 {
		t.Fatalf("ranked = %+v, want t_a with its one resolvable claim", rep.Ranked)
	}
	if !hasRejection(rep, "unknown candidate") {
		t.Fatalf("rejected = %+v, want an unknown candidate", rep.Rejected)
	}
	if !hasDroppedClaim(rep, "it is huge", agentloop.DropUnknownEvidence) {
		t.Fatalf("dropped claims = %+v, want the E999 claim", rep.DroppedClaims)
	}
}

func hasRejection(rep report.Report, reason string) bool {
	for _, r := range rep.Rejected {
		if r.Reason == reason {
			return true
		}
	}
	return false
}

func hasDroppedClaim(rep report.Report, text, reason string) bool {
	for _, d := range rep.DroppedClaims {
		if strings.Contains(d.Text, text) && d.Reason == reason {
			return true
		}
	}
	return false
}

// An ungrounded number in a claim drops it; SQL in prose stays prose and
// an extra sql field is never read.
func TestReview_UngroundedNumbersDropAndSQLIsOnlyProse(t *testing.T) {
	in := input(t, "db1")
	a := keepRanked(t, in, "t_a")
	fact := aliasOf(t, in, "index/idx/public.t_a")
	a.Ranking[0].Claims = append(a.Ranking[0].Claims, claim("t_a holds 99999 bytes", fact))
	a.Hypotheses = []hypothesis{{Title: "drop the table", Kind: "advice", Target: "public.t",
		Rationale: "DROP TABLE public.t; -- it is unused",
		Claims:    []agentloop.Claim{claim("public.t_a had 0 scans", fact)}}}
	a.Extra = []byte(`"DROP TABLE public.t"`)
	rep, err := Review(context.Background(), settings(1, "majority"), deps(always(a), nil), in)
	if err != nil || rep.ModelPass != report.PassDone {
		t.Fatalf("Review: %s %v", rep.ModelPass, err)
	}
	if !hasDroppedClaim(rep, "99999", agentloop.DropUngrounded) {
		t.Fatalf("dropped claims = %+v, want the ungrounded one", rep.DroppedClaims)
	}
	if len(rep.Hypotheses) != 1 || rep.Hypotheses[0].Rationale !=
		"DROP TABLE public.t; -- it is unused" {
		t.Fatalf("hypotheses = %+v, want the SQL-looking rationale kept as prose", rep.Hypotheses)
	}
}

func TestReview_EmptyRepliesFailAndFallBack(t *testing.T) {
	in := input(t, "db1")
	m := failing(llm.ErrEmptyResponse)
	store := &memStore{}
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, store), in)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if rep.ModelPass != report.PassFailed || rep.RunsDone != 0 ||
		!strings.Contains(rep.ModelPassReason, "0 of 3") {
		t.Fatalf("pass = %s %q, want failed with 0 of 3 runs", rep.ModelPass, rep.ModelPassReason)
	}
	checkFallback(t, rep, in)
	runs := store.all()
	if len(runs) != 3 {
		t.Fatalf("stored %d runs, want 3 failed ones", len(runs))
	}
	for _, r := range runs {
		if r.Status != StatusFailed || r.Reason == "" {
			t.Errorf("run = %+v, want failed with a reason", r)
		}
	}
}

func TestReview_NoCandidatesStillReportsTheGrade(t *testing.T) {
	in := input(t, "db1")
	in.Candidates = nil
	m := always(answer{})
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
	if err != nil || rep.ModelPass != report.PassSkipped || m.count() != 0 ||
		len(rep.Ranked) != 0 || len(rep.Sections) != 1 ||
		!strings.Contains(rep.Sections[0].Text, "No candidate") {
		t.Fatalf("no candidates: %+v (%v), want the grade text and no model call", rep, err)
	}
}

func TestReview_NoModelOrOffSkipsAndOnFails(t *testing.T) {
	in := input(t, "db1")
	cases := []struct {
		name   string
		mode   string
		model  agentloop.Model
		status report.PassStatus
		reason string
	}{
		{"off", "off", always(answer{}), report.PassSkipped, "review.model_pass is off"},
		{"auto without a model", "auto", nil, report.PassSkipped, "no model"},
		{"auto with a disabled model", "auto", disabledModel{}, report.PassSkipped, "disabled"},
		{"on without a model", "on", nil, report.PassFailed, "no model"},
	}
	for _, tc := range cases {
		s := settings(3, "majority")
		s.Mode = tc.mode
		rep, err := Review(context.Background(), s, deps(tc.model, nil), in)
		if err != nil || rep.ModelPass != tc.status ||
			!strings.Contains(rep.ModelPassReason, tc.reason) {
			t.Errorf("%s: %s %q (%v), want %s %q", tc.name, rep.ModelPass,
				rep.ModelPassReason, err, tc.status, tc.reason)
			continue
		}
		checkFallback(t, rep, in)
	}
}

type disabledModel struct{}

func (disabledModel) IsEnabled() bool { return false }
func (disabledModel) ChatWithTools(context.Context, []llm.Message, []llm.ToolSpec,
	llm.ToolOptions) (llm.ToolResult, error) {
	return llm.ToolResult{}, llm.ErrLLMDisabled
}

// The model reads only a model tier: a local-tier pack fails the pass
// before any call.
func TestReview_RefusesALocalTierPack(t *testing.T) {
	in := input(t, "db1")
	in.Pack.Manifest.Tier = ""
	m := always(keepRanked(t, in, "t_a"))
	rep, err := Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
	if err != nil || rep.ModelPass != report.PassFailed || m.count() != 0 ||
		!strings.Contains(rep.ModelPassReason, "model tier") {
		t.Fatalf("local tier: %s %q after %d calls (%v)", rep.ModelPass, rep.ModelPassReason,
			m.count(), err)
	}
	in.Pack = nil
	rep, err = Review(context.Background(), settings(3, "majority"), deps(m, nil), in)
	if err != nil || rep.ModelPass != report.PassFailed || m.count() != 0 {
		t.Fatalf("no pack: %s %q (%v)", rep.ModelPass, rep.ModelPassReason, err)
	}
	checkFallback(t, rep, in)
}

func TestReview_InvalidSettingsAreRefused(t *testing.T) {
	in := input(t, "db1")
	for _, s := range []Settings{settings(2, "majority"), settings(0, "majority"),
		settings(7, "majority"), settings(3, "vote"), {Mode: "maybe", Runs: 3,
			Consensus: "majority"}} {
		if _, err := Review(context.Background(), s, deps(nil, nil), in); err == nil {
			t.Errorf("settings %+v were accepted", s)
		}
	}
}

// A store that cannot record a run makes the run unauditable: its output
// is not used and the error says so.
func TestReview_StoreFailureFallsBackAndReturnsTheError(t *testing.T) {
	in := input(t, "db1")
	m := always(keepRanked(t, in, "t_a"))
	rep, err := Review(context.Background(), settings(1, "majority"),
		deps(m, &memStore{err: errTest}), in)
	if err == nil || !strings.Contains(err.Error(), "record") {
		t.Fatalf("err = %v, want the record failure", err)
	}
	if rep.ModelPass != report.PassFailed || !strings.Contains(rep.ModelPassReason, "record") {
		t.Fatalf("pass = %s %q", rep.ModelPass, rep.ModelPassReason)
	}
	checkFallback(t, rep, in)
}
