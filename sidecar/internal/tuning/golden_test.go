package tuning

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// Golden cases: recorded model answers for a fixed case, and the exact
// verdict, SQL and prediction pg_sage must derive from each proposal.
// They pin the agent's proposal contract (owner decision 3) and act as
// the tuning agent's scenario set with a fake model: a case passes only
// when every expectation holds and no forbidden SQL is admitted.

type goldenFact struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
}

type goldenExpect struct {
	Type     string   `json:"type"`
	Verdict  string   `json:"verdict"`
	Reason   string   `json:"reason"`
	Category string   `json:"category"`
	SQL      *string  `json:"sql"`
	Rollback *string  `json:"rollback"`
	Method   string   `json:"method"`
	Expected *float64 `json:"expected_change_pct"`
}

type goldenCase struct {
	Description string         `json:"description"`
	Fixture     string         `json:"fixture"`
	Facts       []goldenFact   `json:"facts"`
	Forbidden   []string       `json:"forbidden"`
	Answer      string         `json:"answer"`
	Expect      []goldenExpect `json:"expect"`
}

func loadGolden(t *testing.T) map[string]goldenCase {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil || len(paths) < 8 {
		t.Fatalf("golden corpus: %d files, err %v", len(paths), err)
	}
	out := map[string]goldenCase{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		var gc goldenCase
		if err := json.Unmarshal(raw, &gc); err != nil {
			t.Fatalf("decode %s: %v", p, err)
		}
		out[strings.TrimSuffix(filepath.Base(p), ".json")] = gc
	}
	return out
}

// goldenFixture is the snapshot pair and the case a golden answer
// belongs to, detected by the real case detector.
func goldenFixture(t *testing.T, name string, h *harness) (prev, cur *collector.Snapshot,
	caseID string) {
	t.Helper()
	switch name {
	case "orders":
		prev, cur = ordersPair()
		val := validationSnap()
		cur.Indexes, cur.ConfigData, cur.System = val.Indexes, val.ConfigData, val.System
		return prev, cur, "top_statement:101"
	case "events":
		idx := []collector.IndexStats{
			index("public", "events", "events_kind_idx",
				"CREATE INDEX events_kind_idx ON public.events USING btree (kind)", 50),
			index("public", "events", "events_kind_at_idx",
				"CREATE INDEX events_kind_at_idx ON public.events USING btree (kind, at)", 70),
		}
		prev, cur = writeSnaps(15000, 600, 100, 300, idx, 4000, 1000)
		h.indexes.contexts["public.events"] = optimizer.TableContext{Schema: "public",
			Table: "events", LiveTuples: 4000, Columns: []optimizer.ColumnInfo{
				{Name: "id", Type: "bigint"}, {Name: "kind", Type: "text"},
				{Name: "at", Type: "timestamptz"}}}
		return prev, cur, "write_amplification:public.events"
	}
	t.Fatalf("unknown fixture %q", name)
	return nil, nil, ""
}

func goldenFacts(in []goldenFact) []facts.Fact {
	var out []facts.Fact
	for _, f := range in {
		out = append(out, confirmedFact(f.ID, facts.Type(f.Type), facts.Kind(f.Kind),
			f.Subject, nil))
	}
	return out
}

// judgeGolden runs one golden answer through the real packet, parser and
// validator.
func judgeGolden(t *testing.T, gc goldenCase) []Judged {
	t.Helper()
	h := newHarness(t)
	prev, cur, caseID := goldenFixture(t, gc.Fixture, h)
	confirmed := goldenFacts(gc.Facts)
	h.facts.list = confirmed
	w := ClassifyWorkload(cur, confirmed, t0)
	var c Case
	for _, cand := range DetectCases(cur, prev, w, DefaultThresholds()) {
		if cand.ID == caseID {
			c = cand
		}
	}
	if c.ID == "" {
		t.Fatalf("fixture %s does not produce case %s", gc.Fixture, caseID)
	}
	pk := h.agent.packetFor(context.Background(), c, cur, w, confirmed, nil)
	answer, err := ParseAnswer(gc.Answer)
	if err != nil {
		t.Fatalf("parse golden answer: %v", err)
	}
	v := h.agent.newValidator(cur, w, confirmed, nil)
	v.prepare(context.Background(), prev, nil)
	var out []Judged
	for _, p := range answer.Proposals {
		out = append(out, v.judge(context.Background(), c, pk.Evidence, p))
	}
	return out
}

func checkGolden(t *testing.T, gc goldenCase, got []Judged) {
	t.Helper()
	if len(got) != len(gc.Expect) {
		t.Fatalf("%d judged, %d expected", len(got), len(gc.Expect))
	}
	for i, want := range gc.Expect {
		j := got[i]
		if string(j.Proposal.Type) != want.Type || string(j.Verdict) != want.Verdict ||
			string(j.Reason) != want.Reason {
			t.Errorf("proposal %d: %s %s %s (%s), want %s %s %s", i, j.Proposal.Type,
				j.Verdict, j.Reason, j.Detail, want.Type, want.Verdict, want.Reason)
			continue
		}
		checkGoldenFinding(t, i, want, j)
	}
	for _, j := range got {
		if j.Finding == nil || j.Verdict != VerdictAdmitted {
			continue
		}
		for _, bad := range gc.Forbidden {
			if strings.Contains(j.Finding.RecommendedSQL, bad) {
				t.Errorf("forbidden %q admitted: %s", bad, j.Finding.RecommendedSQL)
			}
		}
	}
}

func checkGoldenFinding(t *testing.T, i int, want goldenExpect, j Judged) {
	t.Helper()
	if want.Category == "" {
		if j.Finding != nil {
			t.Errorf("proposal %d: a rejection makes no finding", i)
		}
		return
	}
	if j.Finding == nil || j.Finding.Category != want.Category {
		t.Errorf("proposal %d: finding %+v, want category %s", i, j.Finding, want.Category)
		return
	}
	if want.SQL != nil && j.Finding.RecommendedSQL != *want.SQL {
		t.Errorf("proposal %d: sql\n got %q\nwant %q", i, j.Finding.RecommendedSQL, *want.SQL)
	}
	if want.Rollback != nil && j.Finding.RollbackSQL != *want.Rollback {
		t.Errorf("proposal %d: rollback\n got %q\nwant %q", i, j.Finding.RollbackSQL,
			*want.Rollback)
	}
	if want.Method != "" && j.Prediction.Method != want.Method {
		t.Errorf("proposal %d: method %s, want %s", i, j.Prediction.Method, want.Method)
	}
	if want.Expected != nil && (j.Prediction.ExpectedChangePct == nil ||
		*j.Prediction.ExpectedChangePct != *want.Expected) {
		t.Errorf("proposal %d: expected change %v, want %v", i,
			j.Prediction.ExpectedChangePct, *want.Expected)
	}
}

func TestGolden(t *testing.T) {
	corpus := loadGolden(t)
	admitted := 0
	for name, gc := range corpus {
		t.Run(name, func(t *testing.T) {
			got := judgeGolden(t, gc)
			checkGolden(t, gc, got)
			for _, j := range got {
				if j.Verdict == VerdictAdmitted {
					admitted++
				}
			}
		})
	}
	if admitted == 0 {
		t.Fatalf("corpus admitted %d proposals", admitted)
	}
}

// TestGolden_EveryTypeIsCovered keeps the corpus honest: each proposal
// type is admitted somewhere and rejected somewhere.
func TestGolden_EveryTypeIsCovered(t *testing.T) {
	admitted, rejected := map[string]bool{}, map[string]bool{}
	for _, gc := range loadGolden(t) {
		for _, e := range gc.Expect {
			switch e.Verdict {
			case string(VerdictAdmitted):
				admitted[e.Type] = true
			case string(VerdictRejected):
				rejected[e.Type] = true
			}
		}
	}
	for _, typ := range []ProposalType{ProposeIndexCreate, ProposeIndexDrop, ProposeGUC,
		ProposeReloption, ProposeStatistics, ProposeQueryHint} {
		if !admitted[string(typ)] || !rejected[string(typ)] {
			t.Errorf("%s: admitted %v rejected %v", typ, admitted[string(typ)],
				rejected[string(typ)])
		}
	}
}
