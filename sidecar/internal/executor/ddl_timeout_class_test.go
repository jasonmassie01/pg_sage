package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// executor-05: a CONCURRENTLY build, a REINDEX CONCURRENTLY and a plain
// VACUUM get their class timeout; every other statement keeps
// safety.ddl_timeout_seconds. No concurrent access tests: these are pure
// reads of an immutable policy snapshot.

func classTimeoutExecutor(index, reindex, vacuum, ddl int) *Executor {
	cfg := config.DefaultConfig()
	cfg.Safety.IndexBuildTimeoutSeconds = index
	cfg.Safety.ReindexTimeoutSeconds = reindex
	cfg.Safety.VacuumTimeoutSeconds = vacuum
	cfg.Safety.DDLTimeoutSeconds = ddl
	return New(nil, cfg, time.Time{}, nopLog)
}

func TestTimeoutClassOfStatements(t *testing.T) {
	cases := map[string]ddlTimeoutClass{
		"CREATE INDEX CONCURRENTLY i ON public.t (a)":                     classIndexBuild,
		"create unique index concurrently i on public.t (a)":              classIndexBuild,
		"CREATE INDEX i ON public.t (a)":                                  classDDL,
		"REINDEX INDEX CONCURRENTLY public.i":                             classReindex,
		"REINDEX (VERBOSE) TABLE CONCURRENTLY public.t":                   classReindex,
		"REINDEX INDEX public.i":                                          classDDL,
		"VACUUM (ANALYZE) public.t":                                       classVacuum,
		"VACUUM public.t":                                                 classVacuum,
		"VACUUM FULL public.t":                                            classDDL,
		"VACUUM (FULL, ANALYZE) public.t":                                 classDDL,
		"ALTER TABLE public.t SET (fillfactor = 90)":                      classDDL,
		"DROP INDEX CONCURRENTLY IF EXISTS public.i":                      classDDL,
		"CREATE STATISTICS public.s (dependencies) ON a, b FROM public.t": classDDL,
		"": classDDL,
		"CREATE INDEX i ON public.t (a) WHERE n = ' CONCURRENTLY '": classDDL,
		"REINDEX INDEX public.i_concurrently":                       classDDL,
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON public.t (a)": classIndexBuild,
		"REINDEX SCHEMA CONCURRENTLY public":                        classReindex,
	}
	for sql, want := range cases {
		if got := timeoutClassOf(sql); got != want {
			t.Errorf("timeoutClassOf(%q) = %s, want %s", sql, got, want)
		}
	}
}

func TestDDLTimeoutForUsesTheClassTimeout(t *testing.T) {
	e := classTimeoutExecutor(7200, 3600, 1800, 300)
	cases := map[string]time.Duration{
		"CREATE INDEX CONCURRENTLY i ON public.t (a)": 2 * time.Hour,
		"REINDEX INDEX CONCURRENTLY public.i":         time.Hour,
		"VACUUM (ANALYZE) public.t":                   30 * time.Minute,
		"ALTER TABLE public.t SET (fillfactor = 90)":  300 * time.Second,
		"CREATE INDEX i ON public.t (a)":              300 * time.Second,
		"VACUUM FULL public.t":                        300 * time.Second,
	}
	for sql, want := range cases {
		if got := e.ddlTimeoutFor(sql); got != want {
			t.Errorf("ddlTimeoutFor(%q) = %s, want %s", sql, got, want)
		}
		if got := e.applyTimeoutFor(sql); got != want+applyGrace {
			t.Errorf("applyTimeoutFor(%q) = %s, want %s", sql, got, want+applyGrace)
		}
	}
}

func TestDDLTimeoutForDefaultsWhenUnset(t *testing.T) {
	e := classTimeoutExecutor(0, 0, 0, 0)
	if got := e.ddlTimeoutFor("CREATE INDEX CONCURRENTLY i ON public.t (a)"); got !=
		4*time.Hour {
		t.Fatalf("unset index build timeout = %s, want the 4h default", got)
	}
	if got := e.ddlTimeoutFor("ALTER TABLE public.t SET (fillfactor = 90)"); got !=
		time.Duration(config.DefaultDDLTimeoutSeconds)*time.Second {
		t.Fatalf("unset ddl timeout = %s, want the 300 s default", got)
	}
}

func TestHorizonsCoverTheLongestClass(t *testing.T) {
	e := classTimeoutExecutor(600, 86400, 1800, 300)
	longest := 86400 * time.Second
	if got := e.maxDDLTimeout(); got != longest {
		t.Fatalf("maxDDLTimeout = %s, want %s", got, longest)
	}
	if got := e.recommendationLease(); got < 2*longest {
		t.Fatalf("recommendation lease = %s, want at least 2x %s", got, longest)
	}
	if got := e.budgetHoldHorizon(); got < 2*longest {
		t.Fatalf("budget hold horizon = %s, want at least 2x %s", got, longest)
	}
	small := classTimeoutExecutor(60, 60, 60, 900)
	if got := small.maxDDLTimeout(); got != 900*time.Second {
		t.Fatalf("maxDDLTimeout with a larger ddl timeout = %s, want 900s", got)
	}
}
