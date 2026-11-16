package changeplan

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeResolver answers Validate's lookups from maps; err fields make a
// lookup fail.
type fakeResolver struct {
	mu        sync.Mutex
	findings  map[int64]bool
	evidence  map[string]bool // packHash + "|" + id
	calls     map[int64]int64
	findErr   error
	evErr     error
	callsErr  error
	gotWindow time.Duration
	// settings are the server's setting names and managed the managed
	// change proposals that exist (plan-02d's landing lookups).
	settings   map[string]bool
	managed    map[int64]bool
	settingErr error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		findings: map[int64]bool{11: true, 12: true, 13: true},
		evidence: map[string]bool{testPack + "|setting/random_page_cost": true},
		calls:    map[int64]int64{101: 5000, 102: 5000},
		settings: map[string]bool{"work_mem": true, "random_page_cost": true,
			"shared_buffers": true, "max_connections": true},
		managed: map[int64]bool{},
	}
}

func (f *fakeResolver) SettingExists(_ context.Context, name string) (bool, error) {
	if f.settingErr != nil {
		return false, f.settingErr
	}
	return f.settings[name], nil
}

func (f *fakeResolver) ManagedChangeExists(_ context.Context, id int64) (bool, error) {
	if f.settingErr != nil {
		return false, f.settingErr
	}
	return f.managed[id], nil
}

func (f *fakeResolver) FindingExists(_ context.Context, id int64) (bool, error) {
	if f.findErr != nil {
		return false, f.findErr
	}
	return f.findings[id], nil
}

func (f *fakeResolver) EvidenceExists(_ context.Context, pack, id string) (bool, error) {
	if f.evErr != nil {
		return false, f.evErr
	}
	return f.evidence[pack+"|"+id], nil
}

func (f *fakeResolver) TargetCalls(_ context.Context, ids []int64,
	window time.Duration) (int64, error) {
	f.mu.Lock()
	f.gotWindow = window
	f.mu.Unlock()
	if f.callsErr != nil {
		return 0, f.callsErr
	}
	var n int64
	for _, id := range ids {
		n += f.calls[id]
	}
	return n, nil
}

var testPack = strings.Repeat("ab", 32)

func ptr[T any](v T) *T { return &v }

// indexStep is a reversible CREATE INDEX step on targets.
func indexStep(ordinal int, name, column string, finding int64, target int64) Step {
	return Step{
		Ordinal:    ordinal,
		Kind:       KindAction,
		ActionType: "create_index_concurrently",
		ProposedSQL: "CREATE INDEX CONCURRENTLY " + name + " ON public.orders (" +
			column + ")",
		RollbackSQL:       "DROP INDEX CONCURRENTLY public." + name,
		FindingID:         ptr(finding),
		TargetQueryIDs:    []int64{target},
		Metric:            MetricBlocksPerCall,
		ExpectedDirection: DirectionDown,
		ExpectedPct:       ptr(40.0),
		ExpectedSource:    SourceHypoPG,
		Guardrails:        []Guardrail{{Metric: MetricTimePerCall, MaxRegressPct: 10}},
		MinEvidence:       MinEvidence{MinCalls: 100, MinSamples: 30},
		DeadlineMinutes:   4320,
	}
}

// settingStep is a reloadable setting read back after it is applied.
func settingStep(ordinal int) Step {
	return Step{
		Ordinal:           ordinal,
		Kind:              KindAction,
		ActionType:        "alter_system_guc",
		Setting:           "random_page_cost",
		ProposedSQL:       "ALTER SYSTEM SET random_page_cost = 1.1",
		RollbackSQL:       "ALTER SYSTEM RESET random_page_cost",
		EvidenceIDs:       []string{"setting/random_page_cost"},
		Metric:            MetricReadback,
		ExpectedDirection: DirectionNone,
		ExpectedSource:    SourceNone,
		DeadlineMinutes:   60,
		Checkpoint:        true,
	}
}

// threeStepPlan: two index creates and one reloadable setting, with edges
// 1->3 and 2->3.
func threeStepPlan() Plan {
	return Plan{
		Goal:      "cut blocks per call on the order lookups",
		Scope:     Scope{Objects: []string{"public.orders"}, QueryIDs: []int64{101, 102}},
		Origin:    OriginPerson,
		OriginRef: "user:7",
		PackHash:  testPack,
		Status:    StatusProposed,
		CreatedBy: "alice",
		Steps: []Step{
			indexStep(1, "idx_a", "customer_id", 11, 101),
			indexStep(2, "idx_b", "created_at", 12, 102),
			settingStep(3),
		},
		Edges: []Edge{{From: 1, To: 3}, {From: 2, To: 3}},
	}
}

// codes lists the problems' codes with their ordinals ("code@N").
func codes(ps []Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Code+"@"+strconv.Itoa(p.Ordinal))
	}
	return out
}

func hasCode(ps []Problem, code string, ordinal int) bool {
	for _, p := range ps {
		if p.Code == code && p.Ordinal == ordinal {
			return true
		}
	}
	return false
}
