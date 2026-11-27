package advisor

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// advisors-06: the sub-advisor prompts name the resolved platform and carry
// the documentation prior of the GUCs they tune.

// walOnlyConfig enables only the WAL sub-advisor; cfgEnv is the global
// cfg.CloudEnvironment that fleet runtimes share.
func walOnlyConfig(cfgEnv string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.CloudEnvironment = cfgEnv
	cfg.Advisor.WALEnabled = true
	cfg.Advisor.ConnectionEnabled = false
	cfg.Advisor.RewriteEnabled = false
	cfg.Advisor.BloatEnabled = false
	return cfg
}

// reviewOnce runs Analyze's review step and returns the prompts it sent.
func reviewOnce(t *testing.T, a *Advisor, capture func() []capturedPrompt,
	snap *collector.Snapshot) []capturedPrompt {
	t.Helper()
	if r := a.review(context.Background(), snap); r.budgetErrors != 0 {
		t.Fatalf("budget errors = %d", r.budgetErrors)
	}
	return capture()
}

func onlyPrompt(t *testing.T, prompts []capturedPrompt) capturedPrompt {
	t.Helper()
	if len(prompts) != 1 {
		t.Fatalf("prompts sent = %d, want 1", len(prompts))
	}
	return prompts[0]
}

func TestPromptPlatform_RDSNamesItsRestrictions(t *testing.T) {
	mgr, capture := capturingManager(t, "[]")
	a := New(nil, walOnlyConfig(""), nil, mgr, noopLog)
	a.WithCloudEnv("rds")
	a.WithPGVersion(170004)
	snap := walSnapshots()
	p := onlyPrompt(t, reviewOnce(t, a, capture, snap))
	for _, want := range []string{
		"rds (managed service)", "ALTER SYSTEM unavailable", "parameter group",
		"not through ALTER DATABASE", "max_wal_size, min_wal_size, wal_level",
		"max_wal_size: Soft upper bound", "Safe range: 512MB..64GB",
		"pg_stat_checkpointer",
	} {
		if !strings.Contains(p.system, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, p.system)
		}
	}
	if strings.Contains(p.system+p.user, "self-managed") {
		t.Errorf("rds prompt still says self-managed:\n%s\n%s", p.system, p.user)
	}
}

func TestPromptPlatform_ConnectionAndBloatCarryPlatformAndDocs(t *testing.T) {
	mgr, capture := capturingManager(t, "[]")
	cfg := walOnlyConfig("")
	cfg.Advisor.WALEnabled, cfg.Advisor.ConnectionEnabled = false, true
	cfg.Advisor.BloatEnabled = true
	a := New(nil, cfg, nil, mgr, noopLog)
	a.WithCloudEnv("azure")
	a.WithPGVersion(180001)
	snap := walSnapshots()
	snap.Tables = []collector.TableStats{{SchemaName: "app", RelName: "orders",
		NLiveTup: 9000, NDeadTup: 3000, TableBytes: 64 << 20}}
	prompts := reviewOnce(t, a, capture, snap)
	if len(prompts) != 2 {
		t.Fatalf("prompts sent = %d, want connection and bloat", len(prompts))
	}
	wants := [][]string{
		{"Safe range: 10..10000"},
		{"autovacuum_vacuum_scale_factor", "autovacuum_worker_slots"},
	}
	for i, p := range prompts {
		if !strings.Contains(p.system, "azure (managed service)") ||
			!strings.Contains(p.system, "ALTER SYSTEM unavailable") {
			t.Errorf("prompt %d lacks the azure note:\n%s", i, p.system)
		}
		for _, w := range wants[i] {
			if !strings.Contains(p.system, w) {
				t.Errorf("prompt %d lacks %q:\n%s", i, w, p.system)
			}
		}
		if strings.Contains(p.system+p.user, "self-managed") {
			t.Errorf("prompt %d says self-managed on azure", i)
		}
	}
}

func TestPromptPlatform_FallsBackToConfigThenSelfManaged(t *testing.T) {
	cases := []struct {
		name, cfgEnv, override, want, absent string
	}{
		{"config env", "cloud-sql", "", "cloud-sql (managed service)", "self-managed"},
		{"override wins", "cloud-sql", "aurora", "aurora (managed service)", "cloud-sql"},
		{"nothing known", "", "", "Platform: self-managed", "managed service)"},
		{"probe unknown", "", "unknown", "Platform: unknown", "ALTER SYSTEM is available"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mgr, capture := capturingManager(t, "[]")
			a := New(nil, walOnlyConfig(c.cfgEnv), nil, mgr, noopLog)
			if c.override != "" {
				a.WithCloudEnv(c.override)
			}
			snap := walSnapshots()
			p := onlyPrompt(t, reviewOnce(t, a, capture, snap))
			if !strings.Contains(p.system, c.want) {
				t.Errorf("system prompt lacks %q:\n%s", c.want, p.system)
			}
			if strings.Contains(p.system, c.absent) {
				t.Errorf("system prompt holds %q:\n%s", c.absent, p.system)
			}
		})
	}
}

// Fleet runtimes share one *config.Config; each advisor must name its own
// platform and version even when they review at the same time.
func TestPromptPlatform_ParallelAdvisorsNameTheirOwn(t *testing.T) {
	shared := walOnlyConfig("self-managed")
	// Back-to-back reviews: with interval 0 each round is due again
	// (review honours the per-sub-advisor schedule, advisors-02).
	shared.Advisor.IntervalSeconds = 0
	type run struct {
		a       *Advisor
		capture func() []capturedPrompt
		want    string
		other   string
	}
	var runs []run
	for _, env := range []string{"rds", ""} {
		mgr, capture := capturingManager(t, "[]")
		a := New(nil, shared, nil, mgr, noopLog)
		r := run{a: a, capture: capture, want: "Platform: self-managed", other: "rds"}
		if env != "" {
			a.WithCloudEnv(env)
			a.WithPGVersion(180000)
			r.want, r.other = "rds (managed service)", "self-managed"
		}
		runs = append(runs, r)
	}
	const rounds = 10
	var wg sync.WaitGroup
	for _, r := range runs {
		wg.Add(1)
		go func(r run) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				snap := walSnapshots()
				r.a.review(context.Background(), snap)
			}
		}(r)
	}
	wg.Wait()
	for _, r := range runs {
		prompts := r.capture()
		if len(prompts) != rounds {
			t.Fatalf("prompts = %d, want %d", len(prompts), rounds)
		}
		for i, p := range prompts {
			if !strings.Contains(p.system, r.want) || strings.Contains(p.system, r.other) {
				t.Fatalf("prompt %d of %q leaked another platform:\n%s", i, r.want, p.system)
			}
		}
	}
}

func TestPlatformNote_EveryManagedProvider(t *testing.T) {
	for _, p := range []string{"rds", "aurora", "cloud-sql", "alloydb", "azure",
		"neon", "supabase", "lakebase"} {
		note := platformNote(p)
		if !IsManagedService(p) {
			t.Fatalf("precondition: %s is managed", p)
		}
		for _, want := range []string{p + " (managed service)", "ALTER SYSTEM unavailable",
			"not through ALTER DATABASE"} {
			if !strings.Contains(note, want) {
				t.Errorf("%s note lacks %q: %s", p, want, note)
			}
		}
		for guc := range restrictedSettings[p] {
			if !strings.Contains(note, guc) {
				t.Errorf("%s note does not name restricted %s: %s", p, guc, note)
			}
		}
		if len(note) > 600 {
			t.Errorf("%s note is %d bytes; keep the prompt bounded", p, len(note))
		}
	}
}

func TestPlatformNote_NormalisesAndFallsBack(t *testing.T) {
	cases := map[string]string{
		"  RDS ":       "rds (managed service)",
		"":             "Platform: self-managed",
		"self-managed": "Platform: self-managed",
		"on-prem":      "Platform: self-managed",
		"unknown":      "Platform: unknown",
		"UNKNOWN":      "Platform: unknown",
	}
	for in, want := range cases {
		if got := platformNote(in); !strings.Contains(got, want) {
			t.Errorf("platformNote(%q) = %q, want it to contain %q", in, got, want)
		}
	}
	if strings.Contains(platformNote("unknown"), "ALTER SYSTEM is available") {
		t.Error("an unidentified provider must not be promised ALTER SYSTEM")
	}
}

// The gate stays the enforcement point: a restricted rds setting the model
// proposes anyway is still withheld after the prompt names the restriction.
func TestPromptPlatform_GateStillWithholdsRestrictedSetting(t *testing.T) {
	findings := parseLLMFindings(`[{"object_identifier":"instance","severity":"warning",`+
		`"rationale":"r","recommended_sql":"ALTER SYSTEM SET max_wal_size = '4GB'"}]`,
		"wal_tuning", noopLog)
	got := GateConfigFindingsHost(findings, HostMemory{}, "rds", "app", nil)
	if len(got) != 1 || got[0].RecommendedSQL != "" || got[0].Severity != "info" {
		t.Fatalf("gate did not withhold restricted max_wal_size on rds: %+v", got)
	}
	if !strings.Contains(got[0].Recommendation, "rds") {
		t.Errorf("withheld finding does not name the platform: %q", got[0].Recommendation)
	}
}

// The review that names rds in the prompt gates the answer for rds too: a
// restricted proposal is withheld there and stays executable self-managed.
func TestReview_PromptAndGateShareThePlatform(t *testing.T) {
	cases := []struct {
		env, note string
		withheld  bool
	}{
		{"rds", "rds (managed service)", true},
		{"self-managed", "Platform: self-managed", false},
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			mgr, capture := capturingManager(t, walAdvice)
			a := New(nil, walOnlyConfig(""), nil, mgr, noopLog)
			a.hist = walFixtureHistory{}
			a.WithCloudEnv(c.env)
			snap := walSnapshots()
			r := a.review(context.Background(), snap)
			got, budget := r.findings, r.budgetErrors
			p := onlyPrompt(t, capture())
			if budget != 0 || !strings.Contains(p.system, c.note) {
				t.Fatalf("budget=%d; system prompt lacks %q:\n%s", budget, c.note, p.system)
			}
			if len(got) != 1 {
				t.Fatalf("findings = %+v, want one", got)
			}
			const sql = "ALTER SYSTEM SET max_wal_size = '4GB'"
			if c.withheld && (got[0].RecommendedSQL != "" || got[0].Severity != "info") {
				t.Errorf("rds kept restricted SQL: %+v", got[0])
			}
			if !c.withheld && got[0].RecommendedSQL != sql {
				t.Errorf("self-managed SQL = %q, want %q", got[0].RecommendedSQL, sql)
			}
		})
	}
}
