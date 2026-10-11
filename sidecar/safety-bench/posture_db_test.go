package safetybench

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeProvider returns a fixed finding set, standing in for the detector
// framework so the scoring and fixtures can be tested without it. Each
// finding names its object, since scoring counts only in-scope objects.
type fakeProvider struct{ fire []PostureFinding }

func (fakeProvider) Name() string { return "fake" }

func (f fakeProvider) Findings(context.Context, *pgxpool.Pool) ([]PostureFinding, error) {
	return append([]PostureFinding(nil), f.fire...), nil
}

func TestPostureFixturesApply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	scenarios, err := PostureScenarios()
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	// NotConnectedProvider: fixtures must apply and nothing is claimed.
	results, err := RunPosture(ctx, pool, scenarios, NotConnectedProvider{})
	if err != nil {
		t.Fatalf("run posture: %v", err)
	}
	if len(results) != len(scenarios) {
		t.Fatalf("got %d results, want %d", len(results), len(scenarios))
	}
	for _, r := range results {
		if r.Connected {
			t.Errorf("%s reported connected with the not-connected provider", r.ID)
		}
		if len(r.MatchedDetectors) != 0 {
			t.Errorf("%s claimed matches with no provider: %v", r.ID, r.MatchedDetectors)
		}
	}
}

func TestPostureScoring_WithProvider(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := newPool(ctx, t)
	scenarios, err := PostureScenarios()
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	prov := fakeProvider{fire: []PostureFinding{
		{DetectorID: "AP-03", Severity: "critical", Object: "sb_ps_exposed.profiles"},
		{DetectorID: "AP-04", Severity: "warning", Object: "sb_ps_policy.notes:notes_all"},
		{DetectorID: "AP-05", Severity: "warning", Object: "sb_ps_definer.whoami()"},
		{DetectorID: "AP-07", Severity: "warning", Object: "sb_ps_public"},
	}}
	results, err := RunPosture(ctx, pool, scenarios, prov)
	if err != nil {
		t.Fatalf("run posture: %v", err)
	}
	byID := map[string]PostureResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	// The four SQL-backed scenarios must match their expected detector.
	for _, id := range []string{"PS-exposed-table", "PS-permissive-policy",
		"PS-definer-function", "PS-public-create"} {
		r := byID[id]
		if !r.Connected {
			t.Errorf("%s not connected", id)
		}
		if len(r.MatchedDetectors) != len(r.Expect) || len(r.MissingDetectors) != 0 {
			t.Errorf("%s matched=%v missing=%v, want all of %v", id,
				r.MatchedDetectors, r.MissingDetectors, r.Expect)
		}
	}
	// The version arm (AP-10) is not fired by the fake provider, so it is
	// recorded missing with its version note.
	v := byID["PS-pgvector-version"]
	if len(v.MissingDetectors) != 1 || v.VersionNote == "" {
		t.Errorf("version arm: missing=%v note=%q", v.MissingDetectors, v.VersionNote)
	}
}
