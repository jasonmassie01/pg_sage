package sre

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The model output contract (AI-SRE-SPEC §11): a reply is parsed from the
// submit_review tool call or from JSON content (bare or fenced), bounded
// in size, and checked against what this turn may cite: the graph's own
// hypotheses, in-scope evidence aliases, catalog probes with typed args.

func toolResult(args string) llm.ToolResult {
	return llm.ToolResult{ToolCalls: []llm.ToolCall{{ID: "c1", Name: reviewToolName,
		Arguments: []byte(args)}}}
}

const minimalReview = `{"ranking":["idle_in_tx_holder","ddl_lock_queue"],"claims":[]}`

func TestParseModelReply_ToolCallAndContentForms(t *testing.T) {
	forms := map[string]llm.ToolResult{
		"tool call":       toolResult(minimalReview),
		"bare content":    {Content: minimalReview},
		"fenced content":  {Content: "```json\n" + minimalReview + "\n```"},
		"prose and fence": {Content: "Here is my review:\n```json\n" + minimalReview + "\n```"},
	}
	for name, res := range forms {
		out, rej := parseModelReply(res)
		if rej != nil {
			t.Fatalf("%s: rejected %s (%s)", name, rej.Reason, rej.Detail)
		}
		if len(out.Ranking) != 2 || out.Ranking[0] != "idle_in_tx_holder" ||
			out.NextProbe != nil || len(out.Claims) != 0 {
			t.Fatalf("%s: parsed %+v", name, out)
		}
	}
}

func TestParseModelReply_Negatives(t *testing.T) {
	cases := map[string]struct {
		res    llm.ToolResult
		reason string
	}{
		"empty":            {llm.ToolResult{}, RejectEmpty},
		"blank content":    {llm.ToolResult{Content: "  \n "}, RejectEmpty},
		"malformed":        {llm.ToolResult{Content: `{"ranking": [`}, RejectMalformed},
		"prose only":       {llm.ToolResult{Content: "the idle session is the cause"}, RejectMalformed},
		"array not object": {llm.ToolResult{Content: `["idle_in_tx_holder"]`}, RejectMalformed},
		"unknown field": {llm.ToolResult{Content: `{"ranking":[],"claims":[],` +
			`"sql":"DROP TABLE orders"}`}, RejectMalformed},
		"wrong types": {llm.ToolResult{Content: `{"ranking":"idle","claims":{}}`},
			RejectMalformed},
		"two tool calls": {llm.ToolResult{ToolCalls: []llm.ToolCall{
			{ID: "a", Name: reviewToolName, Arguments: []byte(minimalReview)},
			{ID: "b", Name: reviewToolName, Arguments: []byte(minimalReview)}}},
			RejectMalformed},
		"oversized tool args": {toolResult(`{"ranking":[],"claims":[],"x":"` +
			strings.Repeat("a", maxModelOutputBytes) + `"}`), RejectOversized},
	}
	for name, c := range cases {
		_, rej := parseModelReply(c.res)
		if rej == nil || rej.Reason != c.reason {
			t.Errorf("%s: rejection = %+v, want %s", name, rej, c.reason)
		}
	}
}

// Boundary: exactly maxModelOutputBytes is parsed, one byte more is not.
func TestParseModelReply_SizeBoundary(t *testing.T) {
	pad := maxModelOutputBytes - len(minimalReview)
	atLimit := minimalReview + strings.Repeat(" ", pad)
	if _, rej := parseModelReply(llm.ToolResult{Content: atLimit}); rej != nil {
		t.Fatalf("output of exactly %d bytes rejected: %+v", len(atLimit), rej)
	}
	_, rej := parseModelReply(llm.ToolResult{Content: atLimit + " "})
	rejection(t, rej, RejectOversized)
}

func checkWith(t *testing.T, s reviewScope, raw string) (modelReview, *ModelRejection) {
	t.Helper()
	out, rej := parseModelReply(llm.ToolResult{Content: raw})
	if rej != nil {
		t.Fatalf("parse %s: %+v", raw, rej)
	}
	return s.check(out)
}

func TestReviewCheck_AcceptsAGroundedReview(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	s := newReviewScope(d, ev, false)
	raw := wireReview{Ranking: []string{"idle_in_tx_holder", "ddl_lock_queue"},
		Claims: []wireClaim{idleClaim("E1")}}.json()
	r, rej := checkWith(t, s, raw)
	if rej != nil {
		t.Fatalf("rejected: %+v", rej)
	}
	if len(r.Ranking) != 2 || r.Ranking[0] != "idle_in_tx_holder" || r.NextProbe != nil ||
		len(r.Claims) != 1 || r.Claims[0].EvidenceIDs[0] != "E1" {
		t.Fatalf("review = %+v", r)
	}
	// The model may reorder a conclusive graph here; the verifier decides.
	if _, rej := checkWith(t, s, `{"ranking":["ddl_lock_queue","idle_in_tx_holder"],`+
		`"claims":[]}`); rej != nil {
		t.Fatalf("reordering rejected at check time: %+v", rej)
	}
}

func TestReviewCheck_RankingNegatives(t *testing.T) {
	_, ev, d := idleChainFixture(t)
	s := newReviewScope(d, ev, false)
	cases := map[string]struct {
		ranking []string
		reason  string
	}{
		"invented node":     {[]string{"idle_in_tx_holder", "cosmic_rays"}, RejectUnknownNode},
		"other family node": {[]string{"idle_in_tx_holder", "inactive_slot"}, RejectOutOfScopeNode},
		"ruled out node": {[]string{"idle_in_tx_holder", "ddl_lock_queue",
			"hot_row_contention"}, RejectOutOfScopeNode},
		"duplicate":  {[]string{"idle_in_tx_holder", "idle_in_tx_holder"}, RejectRanking},
		"incomplete": {[]string{"idle_in_tx_holder"}, RejectRanking},
		"empty":      {[]string{}, RejectRanking},
		"nil":        {nil, RejectRanking},
	}
	for name, c := range cases {
		_, rej := checkWith(t, s, wireReview{Ranking: c.ranking}.json())
		if rej == nil || rej.Reason != c.reason {
			t.Errorf("%s: rejection = %+v, want %s", name, rej, c.reason)
		}
	}
}

func unknownRanking() []string {
	return []string{"idle_in_tx_holder", "ddl_lock_queue", "hot_row_contention"}
}

func TestReviewCheck_NextProbe(t *testing.T) {
	_, ev, d := unknownLockFixture(t)
	s := newReviewScope(d, ev, true)
	start := "2026-10-01T09:00:00Z"
	good := []struct {
		probe string
		args  map[string]any
		want  probes.Args
	}{
		{"lock_graph", nil, probes.Args{}},
		{"backend_identity", map[string]any{"pid": 4242, "backend_start": start},
			probes.Args{PID: 4242, BackendStart: time.Date(2026, 10, 1, 9, 0, 0, 0,
				time.UTC)}},
		{"sage_actions", map[string]any{"window_seconds": 3600},
			probes.Args{Window: time.Hour}},
		{"sage_actions", map[string]any{"window_seconds": 60},
			probes.Args{Window: time.Minute}},
	}
	for _, g := range good {
		raw := wireReview{Ranking: unknownRanking(),
			NextProbe: nextProbe(g.probe, g.args, "see whether the blocker is idle")}.json()
		r, rej := checkWith(t, s, raw)
		if rej != nil {
			t.Fatalf("%s %v rejected: %+v", g.probe, g.args, rej)
		}
		if r.NextProbe == nil || string(r.NextProbe.ID) != g.probe ||
			!r.NextProbe.Args.BackendStart.Equal(g.want.BackendStart) ||
			r.NextProbe.Args.PID != g.want.PID || r.NextProbe.Args.Window != g.want.Window ||
			r.NextProbe.Rationale != "see whether the blocker is idle" {
			t.Fatalf("%s: next probe = %+v", g.probe, r.NextProbe)
		}
	}
}

func TestReviewCheck_NextProbeNegatives(t *testing.T) {
	_, ev, d := unknownLockFixture(t)
	s := newReviewScope(d, ev, true)
	why := "tells idle from active"
	cases := map[string]struct {
		probe map[string]any
		scope reviewScope
		want  string
	}{
		"not in catalog": {nextProbe("drop_table", nil, why), s, RejectUnknownProbe},
		"sql as probe":   {nextProbe("SELECT 1", nil, why), s, RejectUnknownProbe},
		"pid on lock_graph": {nextProbe("lock_graph", map[string]any{"pid": 5}, why), s,
			RejectProbeArgs},
		"sql argument": {nextProbe("lock_graph", map[string]any{"sql": "DROP TABLE t"},
			why), s, RejectProbeArgs},
		"window too short": {nextProbe("sage_actions",
			map[string]any{"window_seconds": 59}, why), s, RejectProbeArgs},
		"bad timestamp": {nextProbe("backend_identity", map[string]any{"pid": 1,
			"backend_start": "yesterday"}, why), s, RejectProbeArgs},
		"pid as text": {nextProbe("backend_identity", map[string]any{"pid": "1",
			"backend_start": "2026-10-01T09:00:00Z"}, why), s, RejectProbeArgs},
		"no rationale": {nextProbe("lock_graph", nil, ""), s, RejectRationale},
		"multi-line":   {nextProbe("lock_graph", nil, "a\nb"), s, RejectRationale},
		"too long":     {nextProbe("lock_graph", nil, strings.Repeat("r", 301)), s, RejectRationale},
		"conclusive graph": {nextProbe("lock_graph", nil, why),
			newReviewScope(d, ev, false), RejectProbeNotAllowed},
	}
	for name, c := range cases {
		raw := wireReview{Ranking: unknownRanking(), NextProbe: c.probe}.json()
		_, rej := checkWith(t, c.scope, raw)
		if rej == nil || rej.Reason != c.want {
			t.Errorf("%s: rejection = %+v, want %s", name, rej, c.want)
		}
	}
	at := wireReview{Ranking: unknownRanking(),
		NextProbe: nextProbe("lock_graph", nil, strings.Repeat("r", 300))}.json()
	if _, rej := checkWith(t, s, at); rej != nil {
		t.Fatalf("a 300-character rationale was rejected: %+v", rej)
	}
}
