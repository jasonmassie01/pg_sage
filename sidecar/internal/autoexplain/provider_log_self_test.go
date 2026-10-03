package autoexplain

import (
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// auto_explain logs every slow statement, pg_sage's own included (perf
// v1.8.3 stopped hiding pg_sage from statistics). A plan of pg_sage's own
// statement is not workload evidence: it is rejected by session name or by
// the statement tag, with a distinguishable error (perf-selfexcl).
func TestParseObservedPlanRejectsPgSageStatements(t *testing.T) {
	plan := func(app, query string) logwatch.LogEntry {
		return logwatch.LogEntry{Database: "postgres", Timestamp: time.Now(),
			Application: app, Message: `duration: 812.5 ms  plan: {"Query Text":` +
				query + `,"Query Identifier":42,"Plan":{"Total Cost":9.5}}`}
	}
	for name, e := range map[string]logwatch.LogEntry{
		"pg_sage session":   plan("pg_sage", `"SELECT count(*) FROM orders"`),
		"tagged statement":  plan("", `"SELECT /* pg_sage */ count(*) FROM orders"`),
		"sage schema query": plan("psql", `"SELECT id FROM sage.findings"`),
	} {
		if _, err := ParseObservedPlan(e, "postgres"); !errors.Is(err, ErrSelfStatement) {
			t.Errorf("%s: err = %v, want ErrSelfStatement", name, err)
		}
	}
	p, err := ParseObservedPlan(plan("orders-api", `"SELECT count(*) FROM orders"`),
		"postgres")
	if err != nil || p.QueryID != 42 || p.ExecutionMS != 812.5 {
		t.Fatalf("application plan = %+v, %v", p, err)
	}
}
