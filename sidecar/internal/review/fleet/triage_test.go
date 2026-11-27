package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// Tier 2: the model triage over the rollup, with deterministic triage
// whenever the model is absent or its answer is unusable.

func seeded(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = 40*i + 3
	}
	return out
}

func rollupsOf(fleet map[string]Rollup) []Rollup {
	return BuildRollups(context.Background(), names(fleet), &mapSource{rollups: fleet}, 4)
}

func outlierNames(os []Outlier) string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.Database)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func wantNames(idx []int) string {
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, fmt.Sprintf("db%04d", i))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestTriage_ScriptedModelNamesTheSeededOutliers(t *testing.T) {
	seeds := seeded(25)
	m := namesDrops(t)
	tr := RunTriage(context.Background(), m, rollupsOf(synth(1000, seeds...)), nil)
	if tr.Mode != ModeModel || outlierNames(tr.Outliers) != wantNames(seeds) ||
		len(tr.Dropped) != 0 || tr.ErrorClass != "" {
		t.Fatalf("triage %s %d outliers dropped %v class %q", tr.Mode, len(tr.Outliers),
			tr.Dropped, tr.ErrorClass)
	}
	for _, o := range tr.Outliers {
		if o.Source != ModeModel || len(o.Cites) != 1 || o.Cites[0] != o.ID ||
			o.Score <= 0 || o.Reason == "" {
			t.Fatalf("outlier %+v lacks its source, citation, score or reason", o)
		}
	}
	if m.calls() < 2 || tr.Usage.Calls != m.calls() || tr.Usage.PromptTokens <= 0 {
		t.Fatalf("%d calls, usage %+v: 1,000 members must be triaged in batches",
			m.calls(), tr.Usage)
	}
	for i, p := range m.purposes {
		if p != PurposeTriage || !strings.Contains(m.prompts[i], `<data label="fleet_rollup">`) {
			t.Fatalf("call %d purpose %q, prompt not wrapped as data", i, p)
		}
		if est := len(m.prompts[i]) / 4; est > TriageBatchTokens {
			t.Fatalf("batch %d is about %d tokens, over %d", i, est, TriageBatchTokens)
		}
	}
}

func TestTriage_MalformedJSONFallsBackWithTheReason(t *testing.T) {
	tr := RunTriage(context.Background(), constModel(`{"outliers": [`, nil),
		rollupsOf(synth(10, 4)), nil)
	if tr.Mode != ModeDeterministic || outlierNames(tr.Outliers) != "db0004" ||
		!strings.Contains(strings.Join(tr.Reasons, ";"), "malformed") {
		t.Fatalf("triage %s %s reasons %v, want deterministic db0004 for malformed JSON",
			tr.Mode, outlierNames(tr.Outliers), tr.Reasons)
	}
	if tr.Outliers[0].Source != ModeDeterministic {
		t.Fatalf("outlier source %q", tr.Outliers[0].Source)
	}
}

func TestTriage_MarkdownFencedJSONIsParsed(t *testing.T) {
	fenced := "Here you go:\n```json\n{\"outliers\": [{\"member\": \"R2\", " +
		"\"reason\": \"big index candidate\", \"cites\": [\"R2\"]}]}\n```"
	tr := RunTriage(context.Background(), constModel(fenced, nil), rollupsOf(synth(10)), nil)
	if tr.Mode != ModeModel || outlierNames(tr.Outliers) != "db0002" ||
		tr.Outliers[0].Reason != "big index candidate" {
		t.Fatalf("triage %s %+v, want db0002 from the fenced reply", tr.Mode, tr.Outliers)
	}
}

func TestTriage_UnknownMembersAndBadCitationsAreDropped(t *testing.T) {
	ans := `{"outliers": [
		{"member": "R3", "reason": "drop", "cites": ["R3"]},
		{"member": "R9999", "reason": "ghost", "cites": ["R9999"]},
		{"member": "db0004", "reason": "a name, not an id", "cites": ["R4"]},
		{"member": "R5", "reason": "uncited", "cites": []},
		{"member": "R6", "reason": "cites a stranger", "cites": ["R6", "R77"]},
		{"member": "R3", "reason": "again", "cites": ["R3"]}]}`
	tr := RunTriage(context.Background(), constModel(ans, nil), rollupsOf(synth(10)), nil)
	if tr.Mode != ModeModel || outlierNames(tr.Outliers) != "db0003" || len(tr.Dropped) != 5 {
		t.Fatalf("triage %s %s dropped %+v, want only db0003 kept and 5 drops", tr.Mode,
			outlierNames(tr.Outliers), tr.Dropped)
	}
	reasons := map[string]string{}
	for _, d := range tr.Dropped {
		reasons[d.Item] += d.Reason + ";"
	}
	for item, want := range map[string]string{"R9999": "unknown member",
		"db0004": "unknown member", "R5": "no citation", "R6": "unknown citation R77",
		"R3": "duplicate"} {
		if !strings.Contains(reasons[item], want) {
			t.Errorf("drop of %s = %q, want %q", item, reasons[item], want)
		}
	}
}

func TestTriage_EmptyReplyIsDeterministicWithTheReason(t *testing.T) {
	for name, m := range map[string]*scriptModel{
		"blank":            constModel("   ", nil),
		"ErrEmptyResponse": constModel("", llm.ErrEmptyResponse),
	} {
		tr := RunTriage(context.Background(), m, rollupsOf(synth(10, 2)), nil)
		if tr.Mode != ModeDeterministic || outlierNames(tr.Outliers) != "db0002" ||
			!strings.Contains(strings.Join(tr.Reasons, ";"), "empty") {
			t.Errorf("%s: triage %s %s reasons %v", name, tr.Mode,
				outlierNames(tr.Outliers), tr.Reasons)
		}
	}
}

func TestTriage_NoModelAndEmptyFleet(t *testing.T) {
	tr := RunTriage(context.Background(), nil, rollupsOf(synth(10, 9)), nil)
	if tr.Mode != ModeDeterministic || outlierNames(tr.Outliers) != "db0009" ||
		!strings.Contains(strings.Join(tr.Reasons, ";"), "no triage model") {
		t.Fatalf("no model: %s %s %v", tr.Mode, outlierNames(tr.Outliers), tr.Reasons)
	}
	m := namesDrops(t)
	tr = RunTriage(context.Background(), m, nil, nil)
	if len(tr.Outliers) != 0 || m.calls() != 0 {
		t.Fatalf("empty fleet: %d outliers after %d calls", len(tr.Outliers), m.calls())
	}
}

func TestTriage_TimeoutFallsBackAndStoresTheClass(t *testing.T) {
	tr := RunTriage(context.Background(), constModel("", context.DeadlineExceeded),
		rollupsOf(synth(10, 1)), nil)
	if tr.Mode != ModeDeterministic || tr.ErrorClass != "timeout" ||
		outlierNames(tr.Outliers) != "db0001" {
		t.Fatalf("timeout: %s class %q %s", tr.Mode, tr.ErrorClass, outlierNames(tr.Outliers))
	}
	tr = RunTriage(context.Background(), constModel("", errors.New("boom")),
		rollupsOf(synth(10, 1)), nil)
	if tr.ErrorClass != "error" || !strings.Contains(strings.Join(tr.Reasons, ";"), "boom") {
		t.Fatalf("other error: class %q reasons %v", tr.ErrorClass, tr.Reasons)
	}
}

func TestTriage_RateLimitFromTheProviderFallsBackWithClassRate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL, APIKey: "test-key",
		Model: "haiku-class", TimeoutSeconds: 5}, func(string, string, ...any) {})
	tr := RunTriage(context.Background(), c, rollupsOf(synth(10, 5)), nil)
	if tr.Mode != ModeDeterministic || tr.ErrorClass != "rate" ||
		outlierNames(tr.Outliers) != "db0005" {
		t.Fatalf("429: %s class %q %s reasons %v", tr.Mode, tr.ErrorClass,
			outlierNames(tr.Outliers), tr.Reasons)
	}
}

func TestTriage_PricedUsageHasACost(t *testing.T) {
	in, out := 1.0, 5.0
	price := llm.PricingFrom(map[string]config.LLMPrice{"haiku-class": {InputPerM: &in,
		OutputPerM: &out}})
	tr := RunTriage(context.Background(), namesDrops(t), rollupsOf(synth(1000, seeded(25)...)),
		price)
	if tr.Usage.CostUSD == nil || *tr.Usage.CostUSD <= 0 || *tr.Usage.CostUSD >= 10 ||
		tr.Usage.Model != "haiku-class" {
		t.Fatalf("usage %+v, want a priced tier-2 cost under $10 (CHECK-36)", tr.Usage)
	}
	unpriced := RunTriage(context.Background(), namesDrops(t), rollupsOf(synth(10, 1)), nil)
	if unpriced.Usage.CostUSD != nil {
		t.Fatalf("unpriced usage has a cost %v", *unpriced.Usage.CostUSD)
	}
}

func TestDeterministicTriage_RanksDropImpactAndCost(t *testing.T) {
	fleet := synth(10)
	usd := func(v float64) *float64 { return &v }
	for i := 1; i <= 10; i++ {
		r := fleet[fmt.Sprintf("db%04d", i)]
		r.MonthlyUSD = usd(10)
		fleet[r.Database] = r
	}
	prev := 95.0
	a := fleet["db0002"]
	a.PrevScore, a.Score = &prev, 80
	fleet["db0002"] = a
	b := fleet["db0005"]
	b.TopImpact = 50
	fleet["db0005"] = b
	c := fleet["db0008"]
	c.MonthlyUSD = usd(60)
	fleet["db0008"] = c
	got := DeterministicTriage(rollupsOf(fleet))
	if len(got) != 3 || got[0].Database != "db0002" || got[1].Database != "db0005" ||
		got[2].Database != "db0008" {
		t.Fatalf("deterministic outliers %+v, want db0002 (drop), db0005 (impact), "+
			"db0008 (cost)", got)
	}
	for _, o := range got {
		if o.Source != ModeDeterministic || len(o.Cites) != 1 || o.Reason == "" {
			t.Fatalf("outlier %+v", o)
		}
	}
}
