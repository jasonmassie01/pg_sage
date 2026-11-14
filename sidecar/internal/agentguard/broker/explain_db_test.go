//go:build cgo

package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Agent EXPLAIN (security-02): plan-only, on the agent's own broker login,
// behind the same gate, proof and column classes as agent_query, audited.

func (f *fixture) explain(t *testing.T, sql string, params ...string) ExplainResult {
	t.Helper()
	res, err := f.broker.Explain(f.ctx(), ExplainRequest{Database: "db", SQL: sql,
		Params: params})
	require.NoError(t, err)
	return res
}

func requirePlan(t *testing.T, res ExplainResult) map[string]any {
	t.Helper()
	require.Equal(t, VerdictExecute, res.Verdict, "%+v", res)
	require.Equal(t, StatusOK, res.Status, "%+v", res)
	var plans []map[string]any
	require.NoError(t, json.Unmarshal(res.PlanJSON, &plans), "plan JSON: %s", res.PlanJSON)
	require.Len(t, plans, 1)
	plan, ok := plans[0]["Plan"].(map[string]any)
	require.True(t, ok, "no Plan node in %s", res.PlanJSON)
	return plan
}

func TestExplainPlansWithoutRunning(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	res := f.explain(t, "SELECT id, name FROM items WHERE id <= 3")
	plan := requirePlan(t, res)
	_, timed := plan["Actual Total Time"]
	require.False(t, timed, "plan-only EXPLAIN must not execute: %s", res.PlanJSON)
	require.Len(t, res.EnvelopeHash, 64)
	req := f.decider.last()
	require.Equal(t, "explain_query", req.Tool)
	require.Equal(t, agentguard.ToolAgent, req.Kind)
	require.Equal(t, decide.CapRead, req.Capability)
	require.Equal(t, []decide.Object{{Schema: f.schema, Relation: "items"}}, req.Objects)
	audit := f.auditRows(t)
	require.Len(t, audit, 1)
	require.Equal(t, VerdictExecute, audit[0].verdict)
	require.Equal(t, -1, audit[0].rowCount, "a plan returns no rows: row_count is NULL")
	require.Equal(t, res.EnvelopeHash, audit[0].envelope)
	query := f.query(t, "SELECT id, name FROM items WHERE id <= 3")
	require.True(t, query.EnvelopeHash != res.EnvelopeHash,
		"an explain and a query of one statement share an envelope")
}

func TestExplainRunsAsTheBrokerRole(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	requirePlan(t, f.explain(t, "SELECT id FROM items"))
	var users []string
	rows, err := f.super.Query(context.Background(), `SELECT usename::text
		FROM pg_stat_activity WHERE application_name = $1`, "pg_sage agent:"+f.p.ID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var u string
		require.NoError(t, rows.Scan(&u))
		users = append(users, u)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{f.role}, users, "the explain session is the agent's login")
}

func TestExplainRefusesWhatTheRoleCannotRead(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	res := f.explain(t, "SELECT id FROM hidden")
	require.Equal(t, StatusFailed, res.Status, "%+v", res)
	require.Equal(t, "42501", res.SQLState)
	require.Empty(t, res.PlanJSON, "no estimate leaks for an ungranted table")
	require.False(t, strings.Contains(res.Message, "hidden"), "message echoes server text")
	audit := f.auditRows(t)
	require.Len(t, audit, 1)
	require.Equal(t, "sqlstate_42501", audit[0].reason)
	// The connection survives the failed transaction for the next call.
	requirePlan(t, f.explain(t, "SELECT id FROM items WHERE id = $1", "2"))
}

func TestExplainKeepsColumnClassesAndRLS(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.classifyColumn(t, "people", "ssn", classify.ClassPII)
	for _, sql := range []string{"SELECT id, ssn FROM people",
		"SELECT id FROM people WHERE ssn = '123-45-6789'"} {
		res := f.explain(t, sql)
		require.Equal(t, VerdictBlocked, res.Verdict, "%s: %+v", sql, res)
		require.Equal(t, string(decide.ReasonClassification), res.ReasonCode, sql)
		require.Empty(t, res.PlanJSON, "%s: a plan over a masked column leaked", sql)
	}
	requirePlan(t, f.explain(t, "SELECT id FROM owned"))
}

func TestExplainRefusesWritesAndUnprovenReads(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	for _, sql := range []string{"DELETE FROM items", "SELECT 1; SELECT 2",
		"SELECT id FROM items FOR UPDATE", "EXPLAIN SELECT 1",
		"SELECT pg_terminate_backend(1)"} {
		res := f.explain(t, sql)
		require.Equal(t, VerdictBlocked, res.Verdict, "%s: %+v", sql, res)
		require.Empty(t, res.PlanJSON, sql)
	}
	require.Equal(t, "5", cell(t, f.query(t, "SELECT count(*) FROM items"), 0, 0))
}

func TestExplainParameters(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	sql := "SELECT name FROM items WHERE id = $1 AND name <> $2"
	requirePlan(t, f.explain(t, sql, "3", "o'k"))
	// Unbound parameters get the generic plan, not a constant-false one.
	plan := requirePlan(t, f.explain(t, sql))
	require.True(t, plan["Node Type"] != "Result",
		"an unbound parameter folded the plan to a constant: %+v", plan)
	requirePlan(t, f.explain(t, sql, "3"))
	_, err := f.broker.Explain(f.ctx(), ExplainRequest{Database: "db", SQL: sql,
		Params: []string{"1", "a", "extra"}})
	require.ErrorIs(t, err, ErrInvalid, "more params than placeholders")
	// The prepared statement is released: the same call works again.
	requirePlan(t, f.explain(t, sql, "4", "x"))
}

func TestExplainDecisionBlocks(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.decider.verdict = decide.Verdict{Reason: agentguard.ReasonFrozen, Step: "D1",
		Detail: "frozen", Fix: "an operator unfreezes the agent"}
	res := f.explain(t, "SELECT id FROM items")
	require.Equal(t, VerdictBlocked, res.Verdict)
	require.Equal(t, "agent_frozen", res.ReasonCode)
	require.Equal(t, 0, f.logins.calls, "a frozen agent logged in")
	require.Len(t, f.auditRows(t), 1)
}

func TestExplainConcurrentCalls(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := f.broker.Explain(f.ctx(), ExplainRequest{Database: "db",
				SQL: "SELECT id FROM items WHERE id = $1", Params: []string{fmt.Sprint(i)}})
			if err != nil || res.Status != StatusOK || len(res.PlanJSON) == 0 {
				errs <- fmt.Sprintf("call %d: %+v %v", i, res, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	require.Len(t, f.auditRows(t), 8)
}

func TestExplainParameterStaysOneLiteralUnderARoleDefault(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	// With standard_conforming_strings off, a backslash escapes the quote
	// that closes an EXECUTE argument, and the rest of the value is SQL.
	exec(t, f.super, fmt.Sprintf("ALTER ROLE %s SET standard_conforming_strings = off",
		ident(f.role)))
	res := f.explain(t, "SELECT id FROM items WHERE name = $1", `x\') --`)
	plan := requirePlan(t, res)
	require.True(t, strings.Contains(string(res.PlanJSON), `) --`),
		"the parameter was split into SQL: %v", plan)
	requirePlan(t, f.explain(t, "SELECT id FROM items WHERE name = $1", `'; SELECT 1; --`))
}
