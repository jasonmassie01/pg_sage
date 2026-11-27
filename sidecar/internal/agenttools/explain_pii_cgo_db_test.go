//go:build cgo

package agenttools

import "testing"

// security-10 / AQ-04 under explain_guard: EXPLAIN ANALYZE of a cast of
// the pii column fails on the server with a message that quotes the
// stored value. ANALYZE needs the parse-tree inspector (cgo builds).
func TestExplainQuery_AnalyzeCastOfPIIColumnIsWithheld(t *testing.T) {
	f := newFixture(t)
	people := f.peopleTable()
	logs := &logCapture{}
	tools := New(f.pool, Options{Log: logs.log})
	_, err := tools.ExplainQuery(f.ctx, ExplainRequest{
		Query: "SELECT id FROM " + people + " WHERE ssn::int = 0", Analyze: true})
	requireWithheld(t, err, logs)
}
