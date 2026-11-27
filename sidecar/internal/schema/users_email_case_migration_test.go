package schema

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-05: sage.users.email becomes case-insensitive. A clean install,
// or an upgrade without case-variant duplicates, gets lower-case emails and
// the users_email_lower unique index. Duplicates leave the index out, log an
// ERROR and open a critical finding naming the colliding ids; the bootstrap
// still succeeds. No concurrent access test: Bootstrap serialises itself on
// its advisory lock, and the migration runs under it.

func captureSchemaLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func emailLowerIndexDef(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var def *string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass(
		'sage.users_email_lower'))`).Scan(&def); err != nil {
		t.Fatalf("reading users_email_lower: %v", err)
	}
	if def == nil {
		return ""
	}
	return *def
}

func insertRawUsers(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	emails ...string) []int {
	t.Helper()
	ids := make([]int, len(emails))
	for i, email := range emails {
		if err := pool.QueryRow(ctx, `INSERT INTO sage.users (email, password, role)
			VALUES ($1, 'x', 'viewer') RETURNING id`, email).Scan(&ids[i]); err != nil {
			t.Fatalf("insert user %q: %v", email, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.users WHERE id = ANY($1)`, ids)
	})
	return ids
}

type emailCaseFinding struct {
	severity, title string
	detail          map[string]any
}

func openEmailCaseFinding(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool) *emailCaseFinding {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT severity, title, detail::text FROM sage.findings
		WHERE category = 'auth_email_case_duplicates' AND status = 'open'`)
	if err != nil {
		t.Fatalf("reading finding: %v", err)
	}
	defer rows.Close()
	var out []emailCaseFinding
	for rows.Next() {
		var f emailCaseFinding
		var detail string
		if err := rows.Scan(&f.severity, &f.title, &detail); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(detail), &f.detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "@") {
			t.Fatalf("finding detail exposes an email: %s", detail)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return &out[0]
	}
	t.Fatalf("%d open email-case findings, want at most 1", len(out))
	return nil
}

func TestUsersEmailCaseMigration_CleanInstallHasIndex(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	def := emailLowerIndexDef(t, ctx, pool)
	if !strings.Contains(def, "UNIQUE INDEX users_email_lower") ||
		!strings.Contains(def, "lower(email)") {
		t.Fatalf("users_email_lower = %q, want a unique index on lower(email)", def)
	}
	ids := insertRawUsers(t, ctx, pool, "clean-install@example.com")
	if _, err := pool.Exec(ctx, `INSERT INTO sage.users (email, password, role)
		VALUES ('Clean-Install@Example.com', 'x', 'viewer')`); err == nil {
		t.Fatalf("a case variant of user %d was accepted with the index in place", ids[0])
	}
}

func TestUsersEmailCaseMigration_UpgradeLowersEmails(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DROP INDEX sage.users_email_lower`); err != nil {
		t.Fatalf("simulating the pre-apiauth-05 schema: %v", err)
	}
	ids := insertRawUsers(t, ctx, pool, "MiXed.Case@Example.COM", "  Spaced@Example.com ",
		"already-lower@example.com")
	bootstrapWithRetry(t, ctx, pool)
	if def := emailLowerIndexDef(t, ctx, pool); !strings.Contains(def, "lower(email)") {
		t.Fatalf("index after upgrade = %q, want users_email_lower", def)
	}
	want := []string{"mixed.case@example.com", "spaced@example.com",
		"already-lower@example.com"}
	for i, id := range ids {
		var email string
		if err := pool.QueryRow(ctx, `SELECT email FROM sage.users WHERE id = $1`,
			id).Scan(&email); err != nil {
			t.Fatal(err)
		}
		if email != want[i] {
			t.Fatalf("user %d email = %q, want %q", id, email, want[i])
		}
	}
	if f := openEmailCaseFinding(t, ctx, pool); f != nil {
		t.Fatalf("finding opened without duplicates: %+v", f)
	}
	bootstrapWithRetry(t, ctx, pool)
}

func TestUsersEmailCaseMigration_DuplicatesSkipIndexAndRaiseFinding(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	if _, err := pool.Exec(ctx, `DROP INDEX sage.users_email_lower`); err != nil {
		t.Fatalf("simulating the pre-apiauth-05 schema: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.findings
			WHERE category = 'auth_email_case_duplicates'`)
		_ = Bootstrap(bg, pool)
	})
	ids := insertRawUsers(t, ctx, pool, "Twin@Example.com", "twin@example.com ",
		"Solo@Example.com")
	logs := captureSchemaLogs(t)

	bootstrapWithRetry(t, ctx, pool)

	if def := emailLowerIndexDef(t, ctx, pool); def != "" {
		t.Fatalf("index created despite duplicates: %q", def)
	}
	var unchanged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.users WHERE email IN
		('Twin@Example.com', 'twin@example.com ', 'Solo@Example.com')`).
		Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged != 3 {
		t.Fatalf("emails rewritten despite duplicates: %d of 3 unchanged", unchanged)
	}
	requireDuplicateLog(t, logs.String(), ids[0], ids[1], ids[2])
	f := openEmailCaseFinding(t, ctx, pool)
	if f == nil || f.severity != "critical" {
		t.Fatalf("finding = %+v, want an open critical finding", f)
	}
	requireFindingNames(t, f, ids[0], ids[1], ids[2])

	// A second bootstrap keeps one finding, not two.
	bootstrapWithRetry(t, ctx, pool)
	if again := openEmailCaseFinding(t, ctx, pool); again == nil {
		t.Fatal("finding closed while duplicates remain")
	}

	requireResolvedAfterOperatorFix(t, ctx, pool, ids[1])
}

// The operator resolves the duplicate: the next bootstrap completes the
// migration and resolves the finding.
func requireResolvedAfterOperatorFix(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool, duplicate int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.users WHERE id = $1`, duplicate); err != nil {
		t.Fatal(err)
	}
	bootstrapWithRetry(t, ctx, pool)
	if def := emailLowerIndexDef(t, ctx, pool); def == "" {
		t.Fatal("index still absent after the duplicate was removed")
	}
	if f := openEmailCaseFinding(t, ctx, pool); f != nil {
		t.Fatalf("finding still open after the duplicate was removed: %+v", f)
	}
}

func requireDuplicateLog(t *testing.T, logs string, a, b, solo int) {
	t.Helper()
	var line string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "level=ERROR") && strings.Contains(l, "sage.users") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no ERROR naming sage.users logged:\n%s", logs)
	}
	for _, id := range []int{a, b} {
		if !strings.Contains(line, fmt.Sprint(id)) {
			t.Fatalf("ERROR does not name user %d: %s", id, line)
		}
	}
	if strings.Contains(line, "@") {
		t.Fatalf("ERROR logs an email address: %s", line)
	}
	if !strings.Contains(line, fmt.Sprintf("[[%d %d]]", a, b)) {
		t.Fatalf("ERROR does not group exactly [%d %d] (user %d has no duplicate): %s",
			a, b, solo, line)
	}
}

func requireFindingNames(t *testing.T, f *emailCaseFinding, a, b, solo int) {
	t.Helper()
	groups, ok := f.detail["duplicate_user_ids"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("finding detail = %v, want one duplicate group", f.detail)
	}
	group, ok := groups[0].([]any)
	if !ok || len(group) != 2 {
		t.Fatalf("duplicate group = %v, want two ids", groups[0])
	}
	got := []int{int(group[0].(float64)), int(group[1].(float64))}
	if got[0] != a || got[1] != b {
		t.Fatalf("finding names %v, want [%d %d] (not %d)", got, a, b, solo)
	}
}
