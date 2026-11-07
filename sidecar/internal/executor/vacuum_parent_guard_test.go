package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// guardLog records the executor's log lines for assertions.
type guardLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *guardLog) logf(component, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, component+": "+fmt.Sprintf(format, args...))
}

func (l *guardLog) contains(fragment string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func vacuumFinding(sql string) analyzer.Finding {
	return analyzer.Finding{Category: "wraparound_freeze", ObjectType: "table",
		ObjectIdentifier: "public.t", Title: "freeze " + sql, RecommendedSQL: sql,
		Detail: map[string]any{"xid_age": int64(210_000_000)}}
}

// vacuum-01, fail closed: without a pool the relkind cannot be read, so a
// VACUUM needs an operator and the reason names the target. The caller's
// detail map is never written (the marker goes on a copy).
func TestVacuumParentGuard_NoPoolRequiresApproval(t *testing.T) {
	log := &guardLog{}
	exec := New(nil, &config.Config{}, time.Time{}, log.logf)
	f := vacuumFinding(`VACUUM (FREEZE) "public"."t";`)
	got, stale := exec.guardVacuumTarget(context.Background(), f)
	if stale {
		t.Fatal("a lookup failure was treated as a stale finding")
	}
	reason := approvalRequiredReason(got)
	if !strings.Contains(reason, `"public"."t"`) {
		t.Fatalf("approval reason = %q, want one naming the target", reason)
	}
	if _, leaked := f.Detail[analyzer.DetailApprovalRequired]; leaked {
		t.Fatal("the guard wrote the caller's detail map")
	}
	if !log.contains(`"public"."t"`) {
		t.Fatalf("log = %v, want the target named", log.lines)
	}
}

// vacuum-01: only VACUUM statements are looked up; anything else passes
// through unchanged, with no query (no pool needed).
func TestVacuumParentGuard_IgnoresNonVacuum(t *testing.T) {
	exec := New(nil, &config.Config{}, time.Time{}, noopExecLog)
	for _, sql := range []string{
		"ANALYZE public.t", "CREATE INDEX CONCURRENTLY i ON public.t (a)",
		"ALTER SYSTEM SET work_mem = '64MB'", "",
	} {
		f := vacuumFinding(sql)
		got, stale := exec.guardVacuumTarget(context.Background(), f)
		if stale || approvalRequiredReason(got) != "" || got.RecommendedSQL != sql {
			t.Fatalf("%q: stale=%v reason=%q", sql, stale, approvalRequiredReason(got))
		}
	}
}

// vacuum-01, invalid input: a VACUUM that names no table (the whole
// database, every partitioned parent included) or several tables cannot
// be resolved to one relation and needs an operator, without a lookup.
func TestVacuumParentGuard_UnresolvableTargetRequiresApproval(t *testing.T) {
	exec := New(nil, &config.Config{}, time.Time{}, noopExecLog)
	for _, sql := range []string{
		"VACUUM", "VACUUM;", "VACUUM (FREEZE);", "VACUUM FREEZE",
		"VACUUM (FREEZE) public.a, public.b", "VACUUM public.a,public.b;",
	} {
		got, stale := exec.guardVacuumTarget(context.Background(), vacuumFinding(sql))
		if stale || approvalRequiredReason(got) == "" {
			t.Fatalf("%q: stale=%v reason=%q, want approval", sql, stale,
				approvalRequiredReason(got))
		}
	}
}

func TestVacuumTargetParsing(t *testing.T) {
	for _, tc := range []struct {
		sql, want string
		ok        bool
	}{
		{`VACUUM (FREEZE) "Public"."MyTable";`, `"Public"."MyTable"`, true},
		{`VACUUM (FREEZE, ANALYZE) "public"."my.table"`, `"public"."my.table"`, true},
		{"VACUUM ANALYZE public.t (a, b)", "public.t", true},
		{"vacuum verbose public.t;", "public.t", true},
		{"VACUUM (FREEZE) ONLY public.p;", "public.p", true},
		{"VACUUM", "", false},
		{"VACUUM (FREEZE) a, b", "", false},
	} {
		got, ok := singleVacuumTarget(tc.sql)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q: (%q, %v), want (%q, %v)", tc.sql, got, ok, tc.want, tc.ok)
		}
	}
}

// No concurrent access test here: the guard holds no state; one read per
// candidate (the DB test runs it against a live server).
