package siem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// deleteAs deletes the auth_audit row id under application_name pg_sage,
// as role when it is not empty (SET ROLE, so the link's db_user is role).
func deleteAs(t *testing.T, pool *pgxpool.Pool, role string, id int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []string{"SET LOCAL application_name = 'pg_sage'"}
	if role != "" {
		stmts = append(stmts, "SET LOCAL ROLE "+pgx.Identifier{role}.Sanitize())
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM sage.auth_audit WHERE id = $1", id); err != nil {
		t.Fatalf("delete audit row %d: %v", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
}

// End to end: the exporter reads pg_sage's role from its own pool, so a
// delete of an audit row by pg_sage's role is routine and the same delete by
// another role under application_name pg_sage is Medium.
func TestExportTrailRemovalSeverityFollowsRole(t *testing.T) {
	pool := freshDB(t, "siem_trail_role")
	ctx := context.Background()
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	mallory := "siem_mallory_" + hex.EncodeToString(b)
	mustExec(t, pool, "CREATE ROLE "+mallory+" NOLOGIN SUPERUSER")
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+mallory) })
	mustExec(t, pool, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('login_succeeded', '{}'), ('login_failed', '{}')`)
	var first, second int64
	if err := pool.QueryRow(ctx, `SELECT min(id), max(id) FROM sage.auth_audit`).
		Scan(&first, &second); err != nil {
		t.Fatalf("read audit ids: %v", err)
	}
	deleteAs(t, pool, "", first)
	deleteAs(t, pool, mallory, second)
	sink := &memSink{name: "soc"}
	if err := exporter(pool, unfenced, SinkConfig{Sink: sink, Chains: []string{"auth_audit"}}).
		RunOnce(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	var own string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&own); err != nil {
		t.Fatalf("current_user: %v", err)
	}
	got := map[string]int{}
	sink.mu.Lock()
	for _, e := range sink.events {
		if e["activity_name"] != "Audit Record Deleted" {
			continue
		}
		user := e["actor"].(map[string]any)["user"].(map[string]any)["name"].(string)
		got[user] = e["severity_id"].(int)
	}
	sink.mu.Unlock()
	if len(got) != 2 || got[own] != SeverityInformational || got[mallory] != SeverityMedium {
		t.Fatalf("delete severities by role = %v (own %q, other %q)", got, own, mallory)
	}
}
