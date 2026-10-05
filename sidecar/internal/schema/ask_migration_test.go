package schema

import (
	"context"
	"strings"
	"testing"
)

// Ask Sage (roadmap phase 3): sage.ask_conversations and sage.ask_messages
// store each user's conversations (answers cascade with their
// conversation); sage.ask_budget_day is the persisted daily token budget,
// one row per (UTC day, actor) plus the database total under actor '*'.
// Additive and idempotent.

func TestAskMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 3; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"ask_conversations", "ask_messages", "ask_budget_day"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'sage' AND table_name = $1`, table).Scan(&n); err != nil ||
			n != 1 {
			t.Fatalf("table %s: %d (%v)", table, n, err)
		}
	}
	if !strings.Contains(ddlAsk, "IF NOT EXISTS") {
		t.Fatal("the Ask Sage DDL is not idempotent")
	}
}

func TestAskTablesConstraints(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.ask_conversations WHERE actor LIKE 'mig:%'")
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.ask_budget_day WHERE actor LIKE 'mig:%'")
	}
	clean()
	t.Cleanup(clean)
	var conv string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.ask_conversations (actor, title)
		VALUES ('mig:1', 'hello') RETURNING id::text`).Scan(&conv); err != nil {
		t.Fatalf("valid conversation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.ask_messages (conversation_id, question,
		answer, status, tokens) VALUES ($1::uuid, 'why?', '{}'::jsonb, 'answered', 10)`,
		conv); err != nil {
		t.Fatalf("valid message: %v", err)
	}
	for name, sql := range askInvalidRows(conv) {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.ask_budget_day (day, actor, tokens)
		VALUES (current_date, 'mig:4', 5)`); err != nil {
		t.Fatalf("valid budget row: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.ask_budget_day (day, actor, tokens)
		VALUES (current_date, 'mig:4', 6)`); err == nil {
		t.Fatal("a duplicate (day, actor) budget row was accepted")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.ask_conversations WHERE id = $1::uuid`,
		conv); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.ask_messages
		WHERE conversation_id = $1::uuid`, conv).Scan(&left); err != nil || left != 0 {
		t.Fatalf("messages left after their conversation: %d (%v)", left, err)
	}
}

// askInvalidRows are inserts the Ask Sage tables must refuse.
func askInvalidRows(conv string) map[string]string {
	return map[string]string{
		"empty actor": `INSERT INTO sage.ask_conversations (actor) VALUES ('')`,
		"long title": `INSERT INTO sage.ask_conversations (actor, title)
		VALUES ('mig:2', repeat('t', 201))`,
		"empty question": `INSERT INTO sage.ask_messages (conversation_id, question, answer,
		status) VALUES ('` + conv + `'::uuid, '', '{}'::jsonb, 'answered')`,
		"unknown status": `INSERT INTO sage.ask_messages (conversation_id, question, answer,
		status) VALUES ('` + conv + `'::uuid, 'q', '{}'::jsonb, 'approved')`,
		"orphan message": `INSERT INTO sage.ask_messages (conversation_id, question, answer,
		status) VALUES (gen_random_uuid(), 'q', '{}'::jsonb, 'answered')`,
		"negative tokens": `INSERT INTO sage.ask_budget_day (day, actor, tokens)
		VALUES (current_date, 'mig:3', -1)`,
	}
}
