package safetybench

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/agentposture"
)

// pgvectorFixedHNSW is the pgvector release whose HNSW fix AP-10 checks.
const pgvectorFixedHNSW = "0.8.4"

// Every scenario's expected detector fires on its own objects when the
// real detector framework is wired. AP-10's pgvector arm fires only while
// the installed pgvector lacks the HNSW fix; past it, AP-10 must stay quiet.
func TestPostureScenarios_RealDetectorsMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newPool(ctx, t)
	scenarios, err := PostureScenarios()
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	results, err := RunPosture(ctx, pool, scenarios, NewDetectorProvider())
	if err != nil {
		t.Fatalf("run posture: %v", err)
	}
	if len(results) != len(scenarios) {
		t.Fatalf("got %d results, want %d", len(results), len(scenarios))
	}
	vulnerable := pgvectorBelow(ctx, t, pool, pgvectorFixedHNSW)
	for _, r := range results {
		if !r.Connected || r.Provider != "agentposture" {
			t.Errorf("%s: provider %q connected=%v", r.ID, r.Provider, r.Connected)
		}
		if r.ID == "PS-pgvector-version" && !vulnerable {
			if len(r.MatchedDetectors) != 0 {
				t.Errorf("%s matched %v on a fixed pgvector", r.ID, r.MatchedDetectors)
			}
			continue
		}
		if len(r.MissingDetectors) != 0 || len(r.MatchedDetectors) != len(r.Expect) {
			t.Errorf("%s: matched=%v missing=%v found=%+v, want all of %v", r.ID,
				r.MatchedDetectors, r.MissingDetectors, r.Found, r.Expect)
		}
		for _, f := range r.Found {
			if !strings.Contains(f.Object, scopeOf(scenarios, r.ID)) {
				t.Errorf("%s: finding %+v is outside the scenario's scope", r.ID, f)
			}
		}
	}
}

// After a run no bench agent role is left behind: a leftover registered
// agent role would change other suites' posture (PrincipalsExist).
func TestPostureScenarios_TeardownDropsAgentRoles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool := newPool(ctx, t)
	scenarios, err := PostureScenarios()
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	if _, err := RunPosture(ctx, pool, scenarios, NewDetectorProvider()); err != nil {
		t.Fatalf("run posture: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_roles
		WHERE rolname LIKE 'sage\_agentb\_sb%'`).Scan(&left); err != nil {
		t.Fatalf("count bench agent roles: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d bench agent roles left after the run", left)
	}
}

// A scenario whose scope names no object of its fixture records the
// detector missing even though the detector fires elsewhere.
func TestPostureScenarios_ScopeExcludesOtherObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := newPool(ctx, t)
	all, err := PostureScenarios()
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	var sc PostureScenario
	for _, s := range all {
		if s.ID == "PS-public-create" {
			sc = s
		}
	}
	sc.Scope = "sb_ps_absent"
	results, err := RunPosture(ctx, pool, []PostureScenario{sc}, NewDetectorProvider())
	if err != nil {
		t.Fatalf("run posture: %v", err)
	}
	r := results[0]
	if len(r.MatchedDetectors) != 0 || len(r.MissingDetectors) != 1 || len(r.Found) != 0 {
		t.Fatalf("matched=%v missing=%v found=%+v, want AP-07 missing and nothing found",
			r.MatchedDetectors, r.MissingDetectors, r.Found)
	}
}

func TestDetectorProvider_LiveErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool := newPool(ctx, t)
	bad := DetectorProvider{Config: agentposture.DefaultConfig()}
	bad.Config.ClientPatterns = []string{"("}
	if _, err := bad.Findings(ctx, pool); !errors.Is(err, agentposture.ErrInvalidConfig) {
		t.Errorf("invalid client pattern: err = %v, want ErrInvalidConfig", err)
	}
	done, stop := context.WithCancel(ctx)
	stop()
	got, err := NewDetectorProvider().Findings(done, pool)
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Errorf("canceled context: findings=%+v err=%v, want context.Canceled", got, err)
	}
	found, err := NewDetectorProvider().Findings(ctx, pool)
	if err != nil {
		t.Fatalf("live findings: %v", err)
	}
	for _, f := range found {
		if !strings.HasPrefix(f.DetectorID, "AP-") || f.Severity == "" || f.Object == "" {
			t.Errorf("malformed finding %+v", f)
		}
	}
}

func scopeOf(scenarios []PostureScenario, id string) string {
	for _, s := range scenarios {
		if s.ID == id {
			return s.Scope
		}
	}
	return "\x00no scenario"
}

// pgvectorBelow reports whether pgvector is available and its default
// version is older than fixed. Without pgvector the scenario cannot fire.
func pgvectorBelow(ctx context.Context, t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, fixed string) bool {
	t.Helper()
	var v string
	err := pool.QueryRow(ctx, `SELECT default_version FROM pg_catalog.pg_available_extensions
		WHERE name = 'vector'`).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("read the pgvector version: %v", err)
	}
	return versionLess(v, fixed)
}

func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}
