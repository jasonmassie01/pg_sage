package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Ask Sage end to end through the production adapters (post-test audit:
// the ask package's tests use a recording Proposer). A scripted model asks
// to propose a finding; the real executor asks its standing policy gate
// (an autonomous profile that would execute the change on its own) and
// only queues it for a person: the queue item is pending, the index does
// not exist and nothing was logged as executed.

type scriptedLLM struct {
	mu    sync.Mutex
	calls int
	reply func(n int) map[string]any
}

func (s *scriptedLLM) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		s.mu.Lock()
		n := s.calls
		s.calls++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{
			"finish_reason": "stop", "message": s.reply(n)}},
			"usage": map[string]int{"total_tokens": 200}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func toolCallMessage(name, args string) map[string]any {
	return map[string]any{"role": "assistant", "content": "", "tool_calls": []map[string]any{{
		"id": "c1", "type": "function", "function": map[string]any{"name": name,
			"arguments": args}}}}
}

func TestAskIntegration_ProposalIsQueuedByTheRealGateAndNeverExecuted(t *testing.T) {
	pool := autonomyPool(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`DROP TABLE IF EXISTS public.ask_it`)
	exec(`CREATE TABLE public.ask_it (id int, c int)`)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.action_queue WHERE proposed_sql LIKE '%ask_it%'`)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.findings WHERE object_identifier = 'public.ask_it'`)
		_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS public.ask_it`)
	})
	var fid int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail, recommended_sql, rollback_sql) VALUES
		('missing_index', 'warning', 'table', 'public.ask_it', 'Missing index on ask_it',
		'{}'::jsonb, 'CREATE INDEX CONCURRENTLY ask_it_c ON public.ask_it (c)',
		'DROP INDEX CONCURRENTLY public.ask_it_c') RETURNING id`).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	ex := autonomousExecutor(pool)
	ex.WithActionStore(store.NewActionStore(pool), "auto")
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex.EnableStandingPolicyDocument(doc, nil)

	model := &scriptedLLM{reply: func(n int) map[string]any {
		if n == 0 {
			return toolCallMessage("propose_action", fmt.Sprintf(`{"finding_id":%d}`, fid))
		}
		return toolCallMessage("answer", `{"claims":[],"not_observed":["Queued it."]}`)
	}}
	srv := model.serve(t)
	svc, err := ask.New(ask.Deps{Database: "orders", Pool: pool, Config: config.DefaultConfig().Ask,
		Proposer: askProposer{inner: ex}, Model: llm.New(&config.LLMConfig{Enabled: true,
			Endpoint: srv.URL, APIKey: "k", Model: "m", TimeoutSeconds: 5,
			TokenBudgetDaily: 1_000_000}, func(string, string, ...any) {})})
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.Ask(ctx, ask.Caller{Actor: "user:it", MayPropose: true},
		ask.Request{Question: "Propose the fix for the missing index on ask_it"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(a.Actions) != 1 || a.Actions[0].Status != ask.ActionQueued ||
		a.Actions[0].RollbackSQL != "DROP INDEX CONCURRENTLY public.ask_it_c" ||
		!strings.Contains(string(a.Actions[0].Prediction), "index_create") {
		t.Fatalf("actions = %+v", a.Actions)
	}
	var status string
	var decidedBy *int
	if err := pool.QueryRow(ctx, `SELECT status, decided_by FROM sage.action_queue
		WHERE id = $1::int`, a.Actions[0].ID).Scan(&status, &decidedBy); err != nil ||
		status != "pending" || decidedBy != nil {
		t.Fatalf("queue item %s: %q %v (%v)", a.Actions[0].ID, status, decidedBy, err)
	}
	var created, logged int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class
		WHERE relname = 'ask_it_c'), (SELECT count(*) FROM sage.action_log
		WHERE finding_id = $1)`, fid).Scan(&created, &logged); err != nil ||
		created != 0 || logged != 0 {
		t.Fatalf("index created %d, actions logged %d (%v)", created, logged, err)
	}
	_, _ = pool.Exec(ctx, `DELETE FROM sage.ask_conversations WHERE actor = 'user:it'`)
}
