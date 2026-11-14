package schema

import (
	"context"
	"strings"
	"testing"
)

// mcp-07b: sage.ask_runs on a real server. Bootstrap twice is clean, the
// checks refuse bad rows, and a run goes with its conversation. No
// concurrent-writer test: Bootstrap holds its advisory lock.

func TestAskRunsMigration_BootstrapTwiceAndConstraints(t *testing.T) {
	pool := freshSchemaPool(t, "askruns")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
		if _, err := pool.Exec(ctx, ddlAskRuns); err != nil {
			t.Fatalf("step run %d: %v", run, err)
		}
	}
	var conv string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.ask_conversations (actor, title)
		VALUES ('user:1', 't') RETURNING id::text`).Scan(&conv); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	insert := func(state, question, progress, errText string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.ask_runs (id, conversation_id, actor,
			question, read_only, state, progress, error, instance)
			VALUES (gen_random_uuid(), $1::uuid, 'user:1', $2, false, $3, $4::jsonb,
			NULLIF($5, ''), 'host-a')`, conv, question, state, progress, errText)
		return err
	}
	if err := insert("queued", "why?", "[]", ""); err != nil {
		t.Fatalf("a valid run was refused: %v", err)
	}
	var progress string
	var finished bool
	if err := pool.QueryRow(ctx, `SELECT progress::text, finished_at IS NULL
		FROM sage.ask_runs`).Scan(&progress, &finished); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if progress != "[]" || !finished {
		t.Fatalf("progress = %s, finished_at null = %v", progress, finished)
	}
	refuseBadRuns(t, insert)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.ask_conversations`); err != nil {
		t.Fatalf("delete conversation: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.ask_runs`).Scan(&left); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if left != 0 {
		t.Fatalf("%d runs outlived their conversation", left)
	}
}

// refuseBadRuns checks that the table's checks refuse each bad row.
func refuseBadRuns(t *testing.T, insert func(state, question, progress, errText string) error) {
	t.Helper()
	steps := "[" + strings.TrimSuffix(strings.Repeat(`{"tool":"x"},`, 65), ",") + "]"
	bad := map[string]func() error{
		"unknown state":  func() error { return insert("paused", "q", "[]", "") },
		"empty question": func() error { return insert("queued", "", "[]", "") },
		"8001 rune question": func() error {
			return insert("queued", strings.Repeat("q", 8001), "[]", "")
		},
		"65 progress steps": func() error { return insert("queued", "q", steps, "") },
		"progress object":   func() error { return insert("queued", "q", "{}", "") },
		"501 rune error": func() error {
			return insert("failed", "q", "[]", strings.Repeat("e", 501))
		},
	}
	for name, try := range bad {
		if err := try(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
