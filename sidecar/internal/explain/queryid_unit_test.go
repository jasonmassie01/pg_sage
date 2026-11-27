package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/planfresh"
)

// tuning-10: query_id resolution without a server (fake Querier).

type fakeRow struct {
	text *string
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	p, ok := dest[0].(**string)
	if !ok {
		return fmt.Errorf("fakeRow: want **string, got %T", dest[0])
	}
	*p = r.text
	return nil
}

type fakeQuerier struct {
	row  fakeRow
	sql  string
	args []any
}

func (q *fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeQuerier: Query not expected")
}

// QueryRow answers the extension-schema lookup (security-01c-q qualifies
// pg_stat_statements by its schema) with public, and records any other
// statement.
func (q *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "pg_catalog.pg_extension") {
		return schemaRow{}
	}
	q.sql, q.args = sql, args
	return q.row
}

type schemaRow struct{}

func (schemaRow) Scan(dest ...any) error {
	for i, d := range dest {
		p, ok := d.(*string)
		if !ok {
			return fmt.Errorf("schemaRow: want *string, got %T", d)
		}
		if *p = ""; i == 0 {
			*p = "public"
		}
	}
	return nil
}

func strPtr(s string) *string { return &s }

func TestResolveQueryID_ReadsThisDatabasesText(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{text: strPtr("SELECT 1 FROM t WHERE a = $1")}}
	text, err := ResolveQueryID(context.Background(), q, -5191732810777595558)
	if err != nil || text != "SELECT 1 FROM t WHERE a = $1" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	if !strings.Contains(q.sql, "current_database()") || !strings.Contains(q.sql, "dbid") {
		t.Errorf("lookup SQL %q must be scoped to the current database", q.sql)
	}
	if len(q.args) != 1 || q.args[0] != int64(-5191732810777595558) {
		t.Errorf("args = %v, want the id as a bound parameter", q.args)
	}
}

func TestResolveQueryID_UnknownIsInvalid(t *testing.T) {
	_, err := ResolveQueryID(context.Background(), &fakeQuerier{}, 42)
	if !errors.Is(err, ErrExplainInvalidRequest) || !errors.Is(err, ErrUnknownQueryID) {
		t.Fatalf("err = %v, want an invalid request for an unknown id", err)
	}
	if !strings.Contains(err.Error(), "42") || !strings.Contains(err.Error(), "this database") {
		t.Errorf("err = %v, want the id and the database scope named", err)
	}
}

func TestResolveQueryID_PGSSMissingIsInvalidAndSaysWhy(t *testing.T) {
	for _, code := range []string{"42P01", "42883", "55000", "3F000"} {
		q := &fakeQuerier{row: fakeRow{err: fmt.Errorf("wrapped: %w",
			&pgconn.PgError{Code: code, Message: "pg_stat_statements is missing"})}}
		_, err := ResolveQueryID(context.Background(), q, 7)
		if !errors.Is(err, ErrExplainInvalidRequest) || !errors.Is(err, ErrStatementsUnavailable) {
			t.Errorf("code %s: err = %v, want invalid + statements unavailable", code, err)
		}
	}
}

// Error propagation: any other failure is a server error, never a 400.
func TestResolveQueryID_OtherErrorIsNotInvalid(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{err: errors.New("connection reset by peer")}}
	_, err := ResolveQueryID(context.Background(), q, 7)
	if err == nil || errors.Is(err, ErrExplainInvalidRequest) {
		t.Fatalf("err = %v, want a non-request error", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("err = %v, want the cause kept", err)
	}
}

func TestExplain_QueryAndQueryIDTogetherIsInvalid(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	_, err := ex.Explain(context.Background(), ExplainRequest{Query: "SELECT 1", QueryID: 9})
	if !errors.Is(err, ErrExplainInvalidRequest) || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v, want query and query_id rejected together", err)
	}
}

// Nil: without a pool the lookup fails cleanly instead of panicking.
func TestExplain_QueryIDWithoutPool(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	_, err := ex.Explain(context.Background(), ExplainRequest{QueryID: 9})
	if err == nil || errors.Is(err, ErrExplainInvalidRequest) {
		t.Fatalf("err = %v, want a server error naming the missing pool", err)
	}
}

func TestQueryIDJSON_AcceptsNumberAndExactString(t *testing.T) {
	cases := map[string]QueryID{
		`{"query_id": 42}`:                      42,
		`{"query_id": "-5191732810777595558"}`:  -5191732810777595558,
		`{"query_id": "9223372036854775807"}`:   9223372036854775807,
		`{"query_id": -9223372036854775808}`:    -9223372036854775808,
		`{"query": "SELECT 1"}`:                 0,
		`{"query_id": null, "query": "SELECT"}`: 0,
	}
	for body, want := range cases {
		var req ExplainRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if req.QueryID != want {
			t.Errorf("%s: QueryID = %d, want %d", body, req.QueryID, want)
		}
	}
}

func TestQueryIDJSON_RejectsInexactOrJunk(t *testing.T) {
	for _, body := range []string{`{"query_id": 4.2}`, `{"query_id": "42; DROP TABLE x"}`,
		`{"query_id": "9223372036854775808"}`, `{"query_id": true}`, `{"query_id": "0x2a"}`,
		`{"query_id": 1e3}`} {
		var req ExplainRequest
		if err := json.Unmarshal([]byte(body), &req); err == nil {
			t.Errorf("%s: decoded to %d, want an error", body, req.QueryID)
		}
	}
}

// A capture becomes the result with its source and age, and gets the
// rules floor without a database.
func TestCapturedResult_LabelsSourceAndAge(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	now := time.Now()
	ex.now = func() time.Time { return now }
	p := planfresh.Plan{Source: planfresh.SourceCache, Capture: "auto_explain",
		JSON: []byte(spillingPlan), CapturedAt: now.Add(-90 * time.Second), HasActuals: true}
	r := ex.capturedResult(context.Background(), "SELECT 1", p)
	if r.PlanSource != "explain_cache" || r.PlanCapture != "auto_explain" {
		t.Errorf("source=%q capture=%q", r.PlanSource, r.PlanCapture)
	}
	if r.PlanAgeS == nil || *r.PlanAgeS != 90 {
		t.Errorf("plan_age_s = %v, want 90", r.PlanAgeS)
	}
	if r.ActualTimeMs == nil || *r.ActualTimeMs != 41.5 || len(r.SlowBecause) != 1 {
		t.Errorf("actual=%v slow=%v, want 41.5 ms and the spill", r.ActualTimeMs, r.SlowBecause)
	}
	if !strings.Contains(r.Note, "auto_explain") {
		t.Errorf("Note = %q, want it to say the plan was captured", r.Note)
	}
}

// Boundary: a capture "from the future" (clock skew) is age 0, not negative.
func TestCapturedResult_FutureCaptureIsAgeZero(t *testing.T) {
	ex := New(nil, &config.ExplainConfig{}, noopLogFn)
	now := time.Now()
	ex.now = func() time.Time { return now }
	r := ex.capturedResult(context.Background(), "SELECT 1", planfresh.Plan{
		Source: planfresh.SourceCache, Capture: "auto_explain", JSON: []byte(quietPlan),
		CapturedAt: now.Add(5 * time.Second), HasActuals: true})
	if r.PlanAgeS == nil || *r.PlanAgeS != 0 {
		t.Errorf("plan_age_s = %v, want 0", r.PlanAgeS)
	}
}
