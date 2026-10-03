package srebench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Roadmap 1.1 (2026-10-03): a bench report names the pg_sage build it
// scored (version and commit), so the sidecar can refuse a report made
// for another build. CI stamps it from SAGE_BENCH_PG_SAGE_VERSION and
// SAGE_BENCH_PG_SAGE_COMMIT; a local run stamps the running binary's.
// A run on a clone repeats every scenario often enough that each family
// it covers reaches the promotion bar's sample size.

func TestBuildFromEnvNormalizesTheTaggedBuild(t *testing.T) {
	env := map[string]string{EnvPgSageVersion: " v1.8.5 ",
		EnvPgSageCommit: " ABCDEF0123456789abcdef0123456789ABCDEF01 "}
	version, commit := BuildFromEnv(func(k string) string { return env[k] })
	if version != "1.8.5" || commit != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("build = %q %q, want the tag without v and the lower-case commit",
			version, commit)
	}
	version, commit = BuildFromEnv(func(string) string { return "" })
	if version != "" || commit != "" {
		t.Fatalf("unset env stamped %q %q", version, commit)
	}
	// A master push has a commit but no release version.
	env = map[string]string{EnvPgSageCommit: "0123456789abcdef0123456789abcdef01234567"}
	version, commit = BuildFromEnv(func(k string) string { return env[k] })
	if version != "" || commit != env[EnvPgSageCommit] {
		t.Fatalf("master build = %q %q", version, commit)
	}
}

func TestBuildReportStampsThePgSageBuild(t *testing.T) {
	stamped := BuildReport(nil, ReportMeta{Arms: []string{ArmCausalGraph},
		Gated: []string{ArmCausalGraph}, Repeats: 1, GeneratedAt: time.Unix(1_800_000_000, 0),
		PgSageVersion: "1.8.5", PgSageCommit: "0123456789abcdef0123456789abcdef01234567"})
	raw, err := json.Marshal(stamped)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["pg_sage_version"] != "1.8.5" ||
		doc["pg_sage_commit"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("stamped report = %s", raw)
	}
	plain := BuildReport(nil, ReportMeta{Arms: []string{ArmCausalGraph}})
	raw, err = json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	// An unstamped report stays readable by older sidecars: no new keys.
	if strings.Contains(string(raw), "pg_sage_version") ||
		strings.Contains(string(raw), "pg_sage_commit") {
		t.Fatalf("unstamped report carries build keys: %s", raw)
	}
}

// scenariosOf is n sufficient-evidence scenarios of family plus decoys
// (which never count toward top-1).
func scenariosOf(family sre.TriggerKind, sufficient, decoys int) []Scenario {
	var out []Scenario
	for i := 0; i < sufficient; i++ {
		out = append(out, Scenario{ID: string(family) + "-pos", Family: family,
			Class: ClassPositive, Gold: Gold{Root: "root"}})
	}
	for i := 0; i < decoys; i++ {
		out = append(out, Scenario{ID: string(family) + "-decoy", Family: family,
			Class: ClassDecoy})
	}
	return out
}

func TestRepeatsForReachesTheSampleSizeOfEveryFamily(t *testing.T) {
	plan := scenariosOf(sre.TriggerPlan, 3, 2)
	lock := scenariosOf(sre.TriggerLock, 6, 1)
	cases := []struct {
		name      string
		scenarios []Scenario
		minRuns   int
		want      int
	}{
		{"3 sufficient, need 10", plan, 10, 4},
		{"6 sufficient, need 10", lock, 10, 2},
		{"the smallest family decides", append(append([]Scenario{}, lock...), plan...), 10, 4},
		{"exactly divisible", lock, 12, 2},
		{"one more than divisible", lock, 13, 3},
		{"already enough", lock, 6, 1},
		{"zero needed", plan, 0, 1},
		{"negative needed", plan, -5, 1},
		{"capped at the bench maximum", plan, 1000, MaxRepeats},
		{"no scenarios", nil, 10, 1},
		{"decoys only never ask for repeats", scenariosOf(sre.TriggerWAL, 0, 4), 10, 1},
	}
	for _, c := range cases {
		if got := RepeatsFor(c.scenarios, c.minRuns); got != c.want {
			t.Errorf("%s: RepeatsFor = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestGameDayRunConfigCarriesRepeatsAndBuild(t *testing.T) {
	selected := scenariosOf(sre.TriggerPlan, 3, 0)
	opts := GameDayOptions{MinRunsPerFamily: 10, PgSageVersion: "1.8.5",
		PgSageCommit: "0123456789abcdef0123456789abcdef01234567"}
	cfg, meta := gameDayRun(selected, opts, "PostgreSQL 17.2",
		time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	if cfg.Repeats != 4 || meta.Repeats != 4 {
		t.Fatalf("repeats = %d/%d, want 4 so plan_regression reaches n >= 10", cfg.Repeats,
			meta.Repeats)
	}
	if meta.PgSageVersion != "1.8.5" || meta.PgSageCommit != opts.PgSageCommit ||
		meta.ServerVersion != "PostgreSQL 17.2" {
		t.Fatalf("meta = %+v", meta)
	}
	// Game days and local runs never spend model tokens: only the causal
	// graph runs.
	if names := cfg.ArmNames(); len(names) != 1 || names[0] != ArmCausalGraph {
		t.Fatalf("arms = %v", names)
	}
	// Without a sample size a game day runs every scenario once.
	cfg, meta = gameDayRun(selected, GameDayOptions{}, "", time.Time{})
	if cfg.Repeats != 1 || meta.Repeats != 1 || meta.PgSageVersion != "" {
		t.Fatalf("plain game day = %d %+v", cfg.Repeats, meta)
	}
}
