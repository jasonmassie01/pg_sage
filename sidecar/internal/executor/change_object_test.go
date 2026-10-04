package executor

import (
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// The identity one-change-per-object serializes on is derived from the
// statement alone (never from LLM text or evidence): the GUC name for a
// settings change, the relations a statement names for everything else.

func TestChangeGUCNamesTheSetting(t *testing.T) {
	cases := map[string]string{
		"ALTER SYSTEM SET work_mem = '9MB'":                        "work_mem",
		"alter system set WORK_MEM to '10MB';":                     "work_mem",
		`ALTER SYSTEM SET "work_mem" = '10MB'`:                     "work_mem",
		"ALTER SYSTEM RESET work_mem":                              "work_mem",
		"ALTER DATABASE lifeos SET work_mem = '16MB'":              "work_mem",
		"ALTER DATABASE \"Life OS\" RESET random_page_cost":        "random_page_cost",
		"ALTER ROLE app SET work_mem = '32MB'":                     "work_mem",
		"ALTER ROLE app IN DATABASE lifeos SET work_mem TO '32MB'": "work_mem",
		"ALTER SYSTEM RESET ALL":                                   "",
		"CREATE INDEX CONCURRENTLY i ON public.t (a)":              "",
		"ALTER TABLE public.t SET (fillfactor = 90)":               "",
		"ALTER DATABASE lifeos RENAME TO other":                    "",
		"":                                                         "",
		"   ":                                                      "",
	}
	for sql, want := range cases {
		if got := changeGUC(sql); got != want {
			t.Errorf("changeGUC(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestChangeRelationNamesFromStatementAndTargets(t *testing.T) {
	cases := []struct {
		sql     string
		targets []string
		want    []string
	}{
		{"CREATE INDEX CONCURRENTLY idx_memories_live_status_type_quality ON public.memories " +
			"(status, fact_type, quality_score) WHERE valid_to IS NULL",
			[]string{"public.memories|btree(status,fact_type,quality_score)"},
			[]string{"public.memories"}},
		{"DROP INDEX CONCURRENTLY IF EXISTS public.idx_old", nil, []string{"public.idx_old"}},
		{`DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_q"`, []string{"public.idx_q"},
			[]string{`"public"."idx_q"`, "public.idx_q"}},
		{"ALTER TABLE public.events SET (autovacuum_vacuum_scale_factor = 0.02)",
			[]string{"public.events"}, []string{"public.events"}},
		{"VACUUM (FREEZE, ANALYZE) public.orders", nil, []string{"public.orders"}},
		{"ANALYZE public.orders", []string{"public.orders"}, []string{"public.orders"}},
		{"REINDEX INDEX CONCURRENTLY public.idx_x", nil, []string{"public.idx_x"}},
		{"CREATE STATISTICS public.s1 (dependencies) ON a, b FROM public.orders", nil,
			[]string{"public.orders"}},
		// Settings name no relation; "instance" is the GUC finding's target.
		{"ALTER SYSTEM SET work_mem = '10MB'", []string{"instance"}, nil},
		{"", nil, nil},
		{"SELECT 1", []string{"  "}, nil},
	}
	for _, tc := range cases {
		got := changeRelationNames(tc.sql, tc.targets)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("changeRelationNames(%q, %v) = %#v, want %#v", tc.sql, tc.targets,
				got, tc.want)
		}
	}
}

func testWindows() VerificationWindows {
	return VerificationWindows{RollbackWindow: 15 * time.Minute, CreateWindow: time.Hour,
		Cap: 72 * time.Hour, DropWindow: 168 * time.Hour, Grace: time.Hour,
		HoldHorizon: 12 * time.Minute}
}

func TestWaitTimesFollowTheVerificationWindows(t *testing.T) {
	at := time.Date(2026, 10, 4, 17, 55, 0, 0, time.UTC)
	w := testWindows()
	cases := []struct {
		name      string
		sql       string
		now       time.Time
		wantUntil time.Time
		wantHard  time.Time
	}{
		{"guc inside its first window", "ALTER SYSTEM SET work_mem = '9MB'",
			at.Add(time.Minute), at.Add(15 * time.Minute), at.Add(73 * time.Hour)},
		{"guc extended past its first window", "ALTER SYSTEM SET work_mem = '9MB'",
			at.Add(39 * time.Minute), at.Add(73 * time.Hour), at.Add(73 * time.Hour)},
		{"index create", "CREATE INDEX CONCURRENTLY i ON public.memories (a)",
			at.Add(time.Minute), at.Add(time.Hour), at.Add(73 * time.Hour)},
		// Owner decision (PR #122): a drop holds its table only until its first
		// window concludes; its soft-drop monitoring watches the business cycle.
		{"index drop holds only its first window", "DROP INDEX CONCURRENTLY public.i",
			at.Add(time.Minute), at.Add(15 * time.Minute), at.Add(15 * time.Minute)},
		{"index drop after its first window", "DROP INDEX CONCURRENTLY public.i",
			at.Add(time.Hour), at.Add(15 * time.Minute), at.Add(15 * time.Minute)},
		{"reloption", "ALTER TABLE public.t SET (fillfactor = 90)", at,
			at.Add(15 * time.Minute), at.Add(73 * time.Hour)},
	}
	for _, tc := range cases {
		until, hard := waitTimes(tc.sql, at, tc.now, w)
		if !until.Equal(tc.wantUntil) || !hard.Equal(tc.wantHard) {
			t.Errorf("%s: until %s hard %s, want %s / %s", tc.name, until, hard,
				tc.wantUntil, tc.wantHard)
		}
	}
}

// Why a wait ends without a verdict: a drop's first window concluding, or
// the hard deadline for everything else.
func TestWaitReleaseCause(t *testing.T) {
	cases := map[string]string{
		"DROP INDEX CONCURRENTLY public.i":                  policy.ReleaseDropFirstWindow,
		"CREATE INDEX CONCURRENTLY i ON public.t (a)":       policy.ReleaseHardDeadline,
		"ALTER SYSTEM SET work_mem = '9MB'":                 policy.ReleaseHardDeadline,
		"ALTER TABLE public.t SET (fillfactor = 90)":        policy.ReleaseHardDeadline,
		"REINDEX INDEX CONCURRENTLY public.idx_memories_ok": policy.ReleaseHardDeadline,
	}
	for sql, want := range cases {
		if got := waitRelease(sql); got != want {
			t.Errorf("waitRelease(%q) = %q, want %q", sql, got, want)
		}
	}
}

// Partition-tree scope: index, statistics and reloption changes share one
// object across a partitioned table, its partitions and their indexes;
// VACUUM, ANALYZE and settings do not.
func TestPartitionScopedClasses(t *testing.T) {
	cases := map[string]bool{
		"CREATE INDEX CONCURRENTLY i ON public.t (a)":                      true,
		"DROP INDEX CONCURRENTLY public.i":                                 true,
		"REINDEX INDEX CONCURRENTLY public.i":                              true,
		"CREATE STATISTICS public.s (dependencies) ON a, b FROM public.t":  true,
		"ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor = 0.02)": true,
		"VACUUM (FREEZE) public.t":                                         false,
		"ANALYZE public.t":                                                 false,
		"ALTER SYSTEM SET work_mem = '9MB'":                                false,
		"":                                                                 false,
	}
	for sql, want := range cases {
		if got := partitionScoped(sql); got != want {
			t.Errorf("partitionScoped(%q) = %v, want %v", sql, got, want)
		}
	}
}

// A cap configured below the first window never ends the wait before the
// first verdict is due.
func TestWaitTimesCapBelowFirstWindow(t *testing.T) {
	at := time.Date(2026, 10, 4, 17, 55, 0, 0, time.UTC)
	w := testWindows()
	w.Cap = 5 * time.Minute
	_, hard := waitTimes("ALTER SYSTEM SET work_mem = '9MB'", at, at, w)
	if want := at.Add(15*time.Minute + time.Hour); !hard.Equal(want) {
		t.Fatalf("hard %s, want %s", hard, want)
	}
}

// Zero windows (an executor without config) take the documented defaults,
// never a zero-length wait.
func TestVerificationWindowsDefaults(t *testing.T) {
	w := verificationWindowsFor(nil, 0)
	if w.RollbackWindow != 15*time.Minute || w.Cap != 72*time.Hour ||
		w.DropWindow != 168*time.Hour || w.CreateWindow <= 0 || w.Grace != time.Hour ||
		w.HoldHorizon <= 0 {
		t.Fatalf("defaults %+v", w)
	}
}
