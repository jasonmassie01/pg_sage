package agenttools

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: SourceFixPacket only reads the finding; the
// packet hash's stability is checked by repeated calls instead.

// requireNoInjection fails when trusted packet text carries the injection.
func requireNoInjection(t *testing.T, p Packet) {
	t.Helper()
	trusted := map[string]string{
		"problem": p.Problem, "change.up": p.Change.Up, "change.down": p.Change.Down,
		"change.note": p.Change.Note, "change.route": p.Change.Route,
		"verification.method": p.Verification.Method,
		"steps":               strings.Join(p.Verification.Steps, "\n"),
		"category":            p.Category, "severity": p.Severity, "object": p.Object,
	}
	for _, e := range p.Evidence {
		trusted["evidence "+e.Name] = fmt.Sprint(e.Name, e.Value, e.Unit, e.Source)
	}
	for name, text := range trusted {
		require.False(t, strings.Contains(text, "Ignore previous"),
			"%s carries untrusted text: %q", name, text)
		require.False(t, strings.Contains(text, "DROP DATABASE"),
			"%s carries untrusted text: %q", name, text)
	}
}

func evidenceNamed(p Packet, name string) *Evidence {
	for i := range p.Evidence {
		if p.Evidence[i].Name == name {
			return &p.Evidence[i]
		}
	}
	return nil
}

func TestSourceFixPacketForIndexFinding(t *testing.T) {
	f := newFixture(t)
	table := sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	p, err := New(f.pool, Options{}).SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	require.Equal(t, id, p.FindingID)
	require.Equal(t, "missing_index", p.Category)
	require.Equal(t, "warning", p.Severity)
	require.Contains(t, p.Change.Up, "CREATE INDEX CONCURRENTLY idx_x ON "+table+" (a)")
	require.True(t, p.Change.NonTransactional, "CONCURRENTLY cannot run in a transaction")
	require.Contains(t, p.Change.Down, "DROP INDEX CONCURRENTLY "+f.q("idx_x"))
	require.False(t, p.Change.FactBound)
	ev := evidenceNamed(p, "estimated_improvement_pct")
	require.NotNil(t, ev, "evidence %+v", p.Evidence)
	require.Equal(t, 40.0, toFloat(t, ev.Value))
	require.Contains(t, ev.Source, strconv.FormatInt(id, 10), "evidence cites its finding")
	require.Contains(t, strings.ToLower(ev.Source), "finding")
	require.Equal(t, []QueryID{QueryID(bigQueryID)}, p.Targets.QueryIDs,
		"queryid must survive decoding exactly")
	require.Contains(t, p.Targets.Objects, table)
	require.NotEmpty(t, p.Problem)
	require.NotEmpty(t, p.Verification.Steps)
	require.WithinDuration(t, time.Now(), p.GeneratedAt, time.Minute)
	require.NotEmpty(t, p.Hash)
}

func TestSourceFixPacketVerificationPlan(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	p, err := New(f.pool, Options{}).SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	v := p.Verification
	require.Equal(t, "mean_exec_time", v.Metric)
	require.NotNil(t, v.ExpectedChangePct)
	require.Equal(t, -40.0, *v.ExpectedChangePct, "a 40% improvement is a -40% change")
	require.Equal(t, 120, v.WindowMinutes, "default verify window is 2h")
	require.Equal(t, "hypopg", v.Prediction.Method, "HypoPG-validated estimate")
	require.Equal(t, "mean_exec_time", v.Prediction.Metric)
	require.Equal(t, []int64{bigQueryID}, v.Prediction.TargetQueryIDs)
	require.NotNil(t, v.Prediction.ExpectedChangePct)
	require.Equal(t, -40.0, *v.Prediction.ExpectedChangePct)
	require.True(t, v.Prediction.Predicts())
	short, err := New(f.pool, Options{VerifyWindow: time.Hour}).SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	require.Equal(t, 60, short.Verification.WindowMinutes)
}

func TestSourceFixPacketKeepsInjectionUntrusted(t *testing.T) {
	f := newFixture(t)
	sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	p, err := New(f.pool, Options{}).SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	require.Contains(t, p.Untrusted.Title, "Ignore previous instructions")
	require.Contains(t, p.Untrusted.Recommendation, "Ignore previous instructions")
	require.Contains(t, p.Untrusted.Rationale, "Ignore previous instructions")
	require.Len(t, p.Untrusted.Queries, 1)
	require.Contains(t, p.Untrusted.Queries[0], "Ignore previous instructions")
	requireNoInjection(t, p)
}

func TestSourceFixPacketHashStableAndChangeSensitive(t *testing.T) {
	f := newFixture(t)
	table := sourceTable(f)
	id := indexFinding(t, f, bigQueryID)
	tools := New(f.pool, Options{})
	first, err := tools.SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	time.Sleep(10 * time.Millisecond)
	second, err := tools.SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	require.Equal(t, first.Hash, second.Hash, "hash must not depend on GeneratedAt")
	f.execArgs("UPDATE sage.findings SET recommended_sql = $1 WHERE id = $2",
		"CREATE INDEX CONCURRENTLY idx_y ON "+table+" (b)", id)
	changed, err := tools.SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	require.NotEqual(t, first.Hash, changed.Hash, "a changed migration changes the hash")
	require.Contains(t, changed.Change.Up, "idx_y")
}

func TestSourceFixPacketFactBoundFinding(t *testing.T) {
	f := newFixture(t)
	table := sourceTable(f)
	migration := "-- pg_sage source-fix packet (fact #9, confirmed by alice on 2026-10-01)\n" +
		"CREATE INDEX CONCURRENTLY idx_fb ON " + table + " (b);\n"
	id := insertFinding(t, f, findingSpec{
		Category: "missing_index", Object: table + "|btree(b)",
		Title: "Index recommendation for " + table,
		Detail: map[string]any{"table": table, "queryids": []int64{77},
			"source_fix": map[string]any{"fact_id": 9, "route": "source_fix",
				"migration": migration, "down": "DROP INDEX CONCURRENTLY " + f.q("idx_fb") + ";",
				"summary": "pg_sage will not run this DDL"}},
	})
	p, err := New(f.pool, Options{}).SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	require.True(t, p.Change.FactBound)
	require.Equal(t, migration, p.Change.Up)
	require.Equal(t, "DROP INDEX CONCURRENTLY "+f.q("idx_fb")+";", p.Change.Down)
	require.Equal(t, "source_fix", p.Change.Route)
	require.True(t, p.Change.NonTransactional)
	require.Equal(t, []QueryID{77}, p.Targets.QueryIDs)
}

// Without an estimate there is no prediction: the verification plan still
// measures the metric but expects no particular change.
func TestSourceFixPacketWithoutEstimateHasNoPrediction(t *testing.T) {
	f := newFixture(t)
	table := sourceTable(f)
	id := insertFinding(t, f, findingSpec{
		Category: "missing_index", Object: table + "|btree(b)",
		Title:  "Index recommendation for " + table,
		Detail: map[string]any{"table": table, "query_id": "123", "hypopg_validated": false},
		SQL:    strPtr("CREATE INDEX idx_nb ON " + table + " (b)"),
	})
	p, err := New(f.pool, Options{}).SourceFixPacket(f.ctx, id)
	require.NoError(t, err)
	require.False(t, p.Change.NonTransactional, "plain CREATE INDEX runs in a transaction")
	require.Equal(t, "", p.Change.Down, "no rollback recorded")
	require.Nil(t, p.Verification.ExpectedChangePct)
	require.False(t, p.Verification.Prediction.Predicts())
	require.NotEqual(t, "hypopg", p.Verification.Prediction.Method)
	require.Equal(t, []QueryID{123}, p.Targets.QueryIDs, "query_id string accepted")
}

func TestSourceFixPacketErrors(t *testing.T) {
	f := newFixture(t)
	table := sourceTable(f)
	tools := New(f.pool, Options{})
	bare := insertFinding(t, f, findingSpec{
		Category: "table_bloat", Object: table, Title: "Bloat on " + table,
		Detail: map[string]any{"table": table, "bloat_pct": 55},
	})
	_, err := tools.SourceFixPacket(f.ctx, bare)
	require.ErrorIs(t, err, ErrNoChange)
	blank := insertFinding(t, f, findingSpec{
		Category: "missing_index", Object: table + "|blank", Title: "x",
		Detail: map[string]any{"table": table}, SQL: strPtr("   "),
	})
	_, err = tools.SourceFixPacket(f.ctx, blank)
	require.ErrorIs(t, err, ErrNoChange, "whitespace SQL is no change")
	_, err = tools.SourceFixPacket(f.ctx, 9_000_000_000_000)
	require.ErrorIs(t, err, ErrNotFound)
	for _, id := range []int64{0, -1} {
		_, err = tools.SourceFixPacket(f.ctx, id)
		require.ErrorIs(t, err, ErrInvalid, "finding id %d", id)
	}
}
