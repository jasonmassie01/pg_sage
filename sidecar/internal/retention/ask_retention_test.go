package retention

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// Ask Sage (roadmap phase 3): stored conversations age out after
// ask.retention_days from their last message (their answers cascade);
// the daily budget rows age out on the same window.

func askDays(actions, ask int) *config.Config {
	cfg := allDays(actions)
	cfg.Ask.RetentionDays = ask
	return cfg
}

func TestPurgeRulesAgeOutAskConversations(t *testing.T) {
	cfg := askDays(30, 12)
	found := map[string]bool{}
	for _, rule := range purgeRules(cfg) {
		switch rule.table {
		case "ask_conversations":
			if rule.timeCol != "updated_at" || rule.days != 12 || rule.extra != "" {
				t.Fatalf("ask_conversations rule = %+v", rule)
			}
			found[rule.table] = true
		case "ask_budget_day":
			if rule.timeCol != "day" || rule.days != 12 {
				t.Fatalf("ask_budget_day rule = %+v", rule)
			}
			found[rule.table] = true
		}
	}
	if !found["ask_conversations"] || !found["ask_budget_day"] {
		t.Fatalf("ask purge rules found: %v", found)
	}
}

func TestRun_AskConversationsAgeOutWithTheirAnswers(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("ask_ret")
	conv := func(age string) string {
		var id string
		if err := testPool.QueryRow(ctx, `INSERT INTO sage.ask_conversations (actor, title,
			updated_at) VALUES ($1, 'q', now() - $2::interval) RETURNING id::text`, tag,
			age).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO sage.ask_messages (conversation_id,
			question, answer, status) VALUES ($1::uuid, 'q', '{}'::jsonb, 'answered')`,
			id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old, fresh := conv("40 days"), conv("2 days")
	if _, err := testPool.Exec(ctx, `INSERT INTO sage.ask_budget_day (day, actor, tokens)
		VALUES (current_date - 40, $1, 10), (current_date, $1, 20)`, tag); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM sage.ask_conversations WHERE actor = $1`, tag)
		_, _ = testPool.Exec(context.Background(),
			`DELETE FROM sage.ask_budget_day WHERE actor = $1`, tag)
	})
	New(testPool, askDays(30, 30), noopLog).Run(ctx)
	for id, kept := range map[string]bool{old: false, fresh: true} {
		n := countWhere(t, ctx, `SELECT count(*) FROM sage.ask_conversations WHERE id = $1::uuid`,
			id)
		m := countWhere(t, ctx, `SELECT count(*) FROM sage.ask_messages
			WHERE conversation_id = $1::uuid`, id)
		if (n == 1) != kept || (m == 1) != kept {
			t.Fatalf("conversation %s kept=%d/%d messages, want %v", id, n, m, kept)
		}
	}
	if got := countWhere(t, ctx, `SELECT count(*) FROM sage.ask_budget_day WHERE actor = $1`,
		tag); got != 1 {
		t.Fatalf("budget rows left = %d, want today's only", got)
	}
}
