package agentposture

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/schema"
)

// Integration with the findings pipeline: a posture finding lands in
// sage.findings as category agent_posture:AP-NN with its fix only as a
// manual script, and resolves once a completed check no longer sees it.
func TestPipeline_PostureFindingsLandAndResolve(t *testing.T) {
	f := newFixture(t)
	if err := schema.Bootstrap(f.ctx, f.pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	f.exec("CREATE TABLE "+f.q("open_t")+" (id int)",
		"GRANT SELECT ON "+f.q("open_t")+" TO "+f.exposed)
	env := f.env(nil)
	var af []analyzer.Finding
	for _, fd := range f.run("AP-03", env).Findings {
		af = append(af, fd.AnalyzerFinding())
	}
	if err := analyzer.UpsertFindings(f.ctx, f.pool, af); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var sqlText, script, level, status string
	err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(recommended_sql, ''),
		detail->>'manual_script', detail->>'proposal_level', status FROM sage.findings
		WHERE category = 'agent_posture:AP-03' AND object_identifier = $1`,
		f.q("open_t")).Scan(&sqlText, &script, &level, &status)
	if err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if sqlText != "" || level != "L1" || status != "open" ||
		!containsText(script, "ENABLE ROW LEVEL SECURITY") {
		t.Fatalf("stored finding: sql %q level %q status %q script %q", sqlText, level,
			status, script)
	}
	f.exec("ALTER TABLE " + f.q("open_t") + " ENABLE ROW LEVEL SECURITY")
	if n := len(f.run("AP-03", env).Findings); n != 0 {
		// Other tests' objects are in other schemas; this fixture's only table is fixed.
		for _, fd := range f.run("AP-03", env).Findings {
			if fd.Object == f.q("open_t") {
				t.Fatalf("still reported after enabling RLS: %+v", fd)
			}
		}
	}
	if err := analyzer.ResolveCleared(f.ctx, f.pool, map[string]bool{},
		"agent_posture:AP-03"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT status FROM sage.findings
		WHERE category = 'agent_posture:AP-03' AND object_identifier = $1`,
		f.q("open_t")).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("status after the fix = %q (%v), want resolved", status, err)
	}
	raw, _ := json.Marshal(af[0].Detail)
	if !containsText(string(raw), `"evidence"`) {
		t.Fatalf("detail lost the evidence: %s", raw)
	}
}

// failingOnce fails its first run with err, then succeeds.
type failingOnce struct {
	countingDetector
	err error
}

func (d *failingOnce) Detect(ctx context.Context, in Input) ([]Finding, error) {
	d.mu.Lock()
	first := d.runs == 0
	d.mu.Unlock()
	if first {
		d.mu.Lock()
		d.runs++
		d.mu.Unlock()
		return nil, d.err
	}
	return d.countingDetector.Detect(ctx, in)
}

// State transitions of the monitor after a failed detector: a transient
// failure (a timeout) runs posture again on the next cycle; a permanent
// one (a missing privilege) waits for the next catalog change or day.
func TestMonitor_RetriesOnlyTransientFailures(t *testing.T) {
	pool, ctx := livePool(t)
	cases := []struct {
		name  string
		err   error
		again bool
	}{
		{"timeout", &pgconn.PgError{Code: "57014", Message: "statement timeout"}, true},
		{"connection lost", errors.New("unexpected EOF"), true},
		{"permission denied", &pgconn.PgError{Code: "42501", Message: "denied"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := time.Now()
			d := &failingOnce{countingDetector: countingDetector{id: "AP-01"}, err: c.err}
			m := newTestMonitor(t, pool, &now, d)
			if _, err := m.Detect(ctx); err != nil {
				t.Fatalf("first Detect: %v", err)
			}
			now = now.Add(time.Minute)
			before := m.st.fp
			got, err := m.Detect(ctx)
			if err != nil {
				t.Fatalf("second Detect: %v", err)
			}
			ran := d.count() == 2
			// A cluster-wide role change by another package could also
			// trigger a run; only a missing rerun is conclusive for "again".
			if c.again && (!ran || len(got) != 1) {
				t.Fatalf("after a %s, runs = %d findings = %d: want a rerun", c.name,
					d.count(), len(got))
			}
			if !c.again && ran && m.st.fp == before {
				t.Fatalf("after a %s posture ran again on an unchanged catalog", c.name)
			}
		})
	}
}
