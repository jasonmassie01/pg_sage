package agenttools

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// security-10: a server error that quotes a value reaches the explain
// tool's caller withheld, with the SQLSTATE kept; the operator log keeps
// the full text. A cast of a literal fails while planning, so this runs in
// every build; AQ-04's cast of people.ssn needs ANALYZE, which needs the
// parse-tree inspector (explain_pii_cgo_db_test.go, cgo builds).
// No concurrent access tests here: explain_error_test.go covers them.

// requireWithheld checks one explain error: no value, SQLSTATE 22P02
// kept, and the full text in the operator log.
func requireWithheld(t *testing.T, err error, logs *logCapture) {
	t.Helper()
	if err == nil {
		t.Fatal("explain succeeded; the statement did not reach the cast")
	}
	if strings.Contains(err.Error(), leakedSSN) {
		t.Fatalf("explain tool error quotes the value: %v", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
		t.Fatalf("error %v does not carry SQLSTATE 22P02", err)
	}
	if !strings.Contains(logs.text(), leakedSSN) {
		t.Errorf("operator log lacks the full server text: %q", logs.text())
	}
}

// peopleTable creates the safety-bench fixture's people table (one pii
// row) in the test's schema.
func (f *fixture) peopleTable() string {
	people := f.schema + ".people"
	f.exec("CREATE TABLE " + people + " (id int PRIMARY KEY, ssn text NOT NULL)")
	f.exec("INSERT INTO " + people + " (id, ssn) VALUES (1, '" + leakedSSN + "')")
	return people
}

func TestExplainQuery_PlanTimeCastErrorIsWithheld(t *testing.T) {
	f := newFixture(t)
	people := f.peopleTable()
	logs := &logCapture{}
	tools := New(f.pool, Options{Log: logs.log})
	_, err := tools.ExplainQuery(f.ctx, ExplainRequest{
		Query: "SELECT id FROM " + people + " WHERE id = '" + leakedSSN + "'::int"})
	requireWithheld(t, err, logs)
}

// A plan-only EXPLAIN of the cast of the column never runs the cast, so
// it plans and logs nothing withheld.
func TestExplainQuery_PlanOnlyColumnCastStillPlans(t *testing.T) {
	f := newFixture(t)
	people := f.peopleTable()
	logs := &logCapture{}
	tools := New(f.pool, Options{Log: logs.log})
	res, err := tools.ExplainQuery(f.ctx, ExplainRequest{
		Query: "SELECT id FROM " + people + " WHERE ssn::int = 0"})
	if err != nil {
		t.Fatalf("plan-only explain failed: %v", err)
	}
	requirePlanJSON(t, res)
	if strings.Contains(logs.text(), leakedSSN) {
		t.Errorf("plan-only explain logged a withheld error: %q", logs.text())
	}
}
