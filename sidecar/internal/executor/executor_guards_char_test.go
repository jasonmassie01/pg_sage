package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// Characterization of the persistent action guards (anti-oscillation, retry
// ceiling, finding resolution) before they move out of executor.go. Each
// test pins the observable database state the guard leaves behind.

func charFinding(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	category, object string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommendation)
		VALUES ($1, 'info', 'table', $2, 'char guard', '{}', 'r') RETURNING id`,
		category, object).Scan(&id); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.findings WHERE id = $1`, id)
	})
	return id
}

func charAction(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	findingID int64, sql, outcome string, age time.Duration) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, finding_id, sql_executed, outcome, executed_at)
		VALUES ('ddl', $1, $2, $3, now() - $4::interval) RETURNING id`,
		findingID, sql, outcome, fmt.Sprintf("%d seconds", int(age.Seconds())),
	).Scan(&id); err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.action_log WHERE id = $1`, id)
	})
	return id
}

func actedOn(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var acted bool
	if err := pool.QueryRow(ctx, `SELECT acted_on_at IS NOT NULL FROM sage.findings
		WHERE id = $1`, id).Scan(&acted); err != nil {
		t.Fatalf("read finding %d: %v", id, err)
	}
	return acted
}

func uniqueCharSQL() string {
	return fmt.Sprintf("ANALYZE public.char_%d", time.Now().UnixNano())
}

func TestCharOscillation_CountsOnlyRecentSuccessfulRepeats(t *testing.T) {
	pool, ctx := requireDB(t)
	e := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	sql := uniqueCharSQL()
	findingID := charFinding(t, ctx, pool, "char_osc", "public.char_osc")
	f := analyzer.Finding{Title: "osc", RecommendedSQL: sql}

	charAction(t, ctx, pool, findingID, sql, "success", time.Minute)
	charAction(t, ctx, pool, findingID, sql, "rollback_skipped", time.Minute)
	charAction(t, ctx, pool, findingID, sql, "failed", time.Minute)
	charAction(t, ctx, pool, findingID, sql, "success", 8*24*time.Hour)
	if e.exceedsOscillationLimit(ctx, f, findingID) {
		t.Fatal("2 recent successes (plus a failure and an 8-day-old success) hit the limit")
	}
	if actedOn(t, ctx, pool, findingID) {
		t.Fatal("finding marked acted_on below the oscillation limit")
	}
	charAction(t, ctx, pool, findingID, sql, "rolled_back", time.Minute)
	if !e.exceedsOscillationLimit(ctx, f, findingID) {
		t.Fatal("3 recent applied repeats did not hit the oscillation limit")
	}
	if !actedOn(t, ctx, pool, findingID) {
		t.Fatal("oscillating finding was not marked acted_on")
	}
}

func TestCharOscillation_NilPoolAndEmptySQLNeverBlock(t *testing.T) {
	e := New(nil, config.DefaultConfig(), time.Time{}, nopLog)
	if e.exceedsOscillationLimit(context.Background(),
		analyzer.Finding{RecommendedSQL: "ANALYZE x"}, 1) {
		t.Fatal("nil pool blocked an action")
	}
	pool, ctx := requireDB(t)
	e = New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	if e.exceedsOscillationLimit(ctx, analyzer.Finding{}, 1) {
		t.Fatal("empty SQL blocked an action")
	}
}

func TestCharRetries_CountFailuresOfSameFindingIdentity(t *testing.T) {
	pool, ctx := requireDB(t)
	e := New(pool, config.DefaultConfig(), time.Time{}, nopLog)
	object := fmt.Sprintf("public.char_retry_%d", time.Now().UnixNano())
	previous := charFinding(t, ctx, pool, "char_retry", object)
	current := charFinding(t, ctx, pool, "char_retry", object)
	other := charFinding(t, ctx, pool, "char_retry", object+"_other")
	sql := uniqueCharSQL()

	charAction(t, ctx, pool, previous, sql, "failed", time.Minute)
	charAction(t, ctx, pool, current, sql, "failed", time.Minute)
	charAction(t, ctx, pool, other, sql, "failed", time.Minute)
	charAction(t, ctx, pool, current, sql, "success", time.Minute)
	if e.exceedsMaxRetries(ctx, current) {
		t.Fatal("2 failures of this identity (other object ignored) hit the ceiling")
	}
	if actedOn(t, ctx, pool, current) {
		t.Fatal("finding marked acted_on below the retry ceiling")
	}
	charAction(t, ctx, pool, previous, sql, "failed", time.Minute)
	if !e.exceedsMaxRetries(ctx, current) {
		t.Fatal("3 failures across the finding identity did not hit the ceiling")
	}
	if !actedOn(t, ctx, pool, current) || actedOn(t, ctx, pool, other) {
		t.Fatal("retry ceiling marked the wrong findings acted_on")
	}
}

func TestCharMarkFindingActioned_ResolvesAndLinksAction(t *testing.T) {
	pool, ctx := requireDB(t)
	logs := &charLog{}
	e := New(pool, config.DefaultConfig(), time.Time{}, logs.log)
	findingID := charFinding(t, ctx, pool, "char_mark", "public.char_mark")
	actionID := charAction(t, ctx, pool, findingID, uniqueCharSQL(), "success", 0)

	e.markFindingActioned(ctx, findingID, actionID)
	var status string
	var linked int64
	var resolved, acted bool
	if err := pool.QueryRow(ctx, `SELECT status, action_log_id,
		resolved_at IS NOT NULL, acted_on_at IS NOT NULL
		FROM sage.findings WHERE id = $1`, findingID).Scan(
		&status, &linked, &resolved, &acted); err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if status != "resolved" || linked != actionID || !resolved || !acted {
		t.Fatalf("finding = status %q action %d resolved %v acted %v", status, linked,
			resolved, acted)
	}
	if logs.joined() != "" {
		t.Fatalf("successful resolution logged %q", logs.joined())
	}
	e.markFindingActioned(ctx, -1, actionID)
	if !strings.Contains(logs.joined(), "did not resolve finding -1") {
		t.Fatalf("missing finding not reported, logs %q", logs.joined())
	}
}

type charLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *charLog) log(component, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, component+": "+fmt.Sprintf(msg, args...))
}

func (l *charLog) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
