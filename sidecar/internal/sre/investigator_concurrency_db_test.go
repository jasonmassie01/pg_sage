package sre

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Two investigations on one database at once: their probes go through
// the database's one probe runner, so its per-database limiter keeps at
// most one probe on the database at a time, and their model calls draw
// on the same durable daily allocation.

// statelessModel answers by the conversation's progress, so interleaved
// requests of two investigations get the right reply: the first turn of
// an investigation calls two probes, a later turn concludes. Every reply
// reports usage tokens.
func statelessModel(t *testing.T, usage int) *fakeModel {
	reply := func(w http.ResponseWriter, body string) {
		rec := &usageWriter{ResponseWriter: w, usage: usage}
		if strings.Count(body, `"role":"tool"`) == 0 {
			toolCalls(ToolRunProbe, probeArgs(probes.ConnectionSaturation),
				ToolStatView, `{"view":"database"}`)(rec, body)
			return
		}
		submitFixed(invFinal{Outcome: "inconclusive"})(rec, body)
	}
	script := make([]fakeReply, 0, 64)
	for range 64 {
		script = append(script, reply)
	}
	return newFakeModel(t, script...)
}

// usageWriter rewrites the fake's reported usage.
type usageWriter struct {
	http.ResponseWriter
	usage int
}

func (u *usageWriter) Write(b []byte) (int, error) {
	var msg map[string]any
	if json.Unmarshal(b, &msg) != nil {
		return u.ResponseWriter.Write(b)
	}
	msg["usage"] = map[string]int{"prompt_tokens": u.usage - 100,
		"completion_tokens": 100, "total_tokens": u.usage}
	raw, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}
	if _, err := u.ResponseWriter.Write(raw); err != nil {
		return 0, err
	}
	return len(b), nil
}

type interval struct{ start, end time.Time }

func probeIntervals(t *testing.T, st *PostgresStore, inv Investigation) []interval {
	t.Helper()
	ev, err := st.Evidence(t.Context(), inv.Scope, inv.ID)
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	var out []interval
	for _, e := range ev {
		var res probes.Result
		dec := json.NewDecoder(bytes.NewReader(e.Payload))
		dec.UseNumber()
		if err := dec.Decode(&res); err != nil {
			t.Fatalf("payload: %v", err)
		}
		end := res.ObservedAt.Add(time.Duration(res.ElapsedMS) * time.Millisecond)
		out = append(out, interval{res.ObservedAt, end})
	}
	return out
}

func TestInvestigator_TwoInvestigationsShareTheDatabaseProbeLimiter(t *testing.T) {
	st, pool, ctx := liveStore(t, budgetLimits())
	runner := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(4))
	m := statelessModel(t, 300)
	c, _ := investigatorCoordinator(t, ctx, st, runner, m.client(), invOptions{})
	ids := make([]Investigation, 2)
	for i := range 2 {
		inv, _, err := c.Start(ctx, lockTrigger("inv-concurrent-"+itoa(int64(i))))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		ids[i] = inv
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Investigate(ctx, ids[i].ID)
		}()
	}
	wg.Wait()
	var all []interval
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("investigation %d: %v", i, errs[i])
		}
		inv, err := st.Get(ctx, ids[i].Scope, ids[i].ID)
		if err != nil || !inv.State.Terminal() || inv.State == StateFailed {
			t.Fatalf("investigation %d = %s (%v)", i, inv.State, err)
		}
		if run := transcriptOf(t, inv); run.Probes != 2 {
			t.Fatalf("investigation %d ran %d model probes, want 2 (%+v)", i, run.Probes,
				run.Steps)
		}
		all = append(all, probeIntervals(t, st, inv)...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start.Before(all[j].start) })
	for i := 1; i < len(all); i++ {
		if all[i].start.Before(all[i-1].end) {
			t.Fatalf("probes overlapped on one database: %v then %v", all[i-1], all[i])
		}
	}
}

func TestInvestigator_TwoInvestigationsShareTheDailyAllocation(t *testing.T) {
	l := budgetLimits()
	l.DatabaseDailyTokens = 9000
	st, _, ctx := liveStore(t, l)
	m := statelessModel(t, 2000)
	c, _ := investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(), invOptions{})
	var wg sync.WaitGroup
	invs := make([]Investigation, 3)
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			invs[i] = startAndRun(t, ctx, c, lockTrigger("inv-day-"+itoa(int64(i))))
		}()
	}
	wg.Wait()
	scope, _ := c.Scope()
	var held int64
	err := st.pool.QueryRow(context.Background(), `SELECT COALESCE(sum(CASE state
		WHEN 'settled' THEN input_used + output_used + COALESCE(reasoning_used, 0)
		WHEN 'cancelled' THEN 0 ELSE input_reserved + output_reserved + reasoning_reserved
		END), 0)::int8 FROM sage.sre_budget_reservations
		WHERE deployment_id = $1 AND database_id = $2`, string(scope.DeploymentID),
		string(scope.DatabaseID)).Scan(&held)
	if err != nil {
		t.Fatalf("held: %v", err)
	}
	if held > l.DatabaseDailyTokens {
		t.Fatalf("held %d tokens, over the database's daily %d", held, l.DatabaseDailyTokens)
	}
	exhausted := 0
	for _, inv := range invs {
		assertIdleRoot(t, st, inv)
		if run := inv.Summary.Investigator; run != nil && run.Stop == "budget_exhausted" {
			exhausted++
		}
	}
	if exhausted == 0 {
		t.Fatal("no investigation ran out of the shared allocation; the test proves nothing")
	}
}
