package schema

import (
	"context"
	"strings"
	"testing"
)

// pr-01b on a real server: bootstrap twice is clean, sage.source_fix gains
// origin (default 'agent' for rows filed before), repo, pr_number and
// pr_state, and the checks refuse an unknown origin or state.
// No concurrent-writer test: Bootstrap holds its advisory lock.
func TestSourceFixPROriginMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "sfprorigin")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT column_name, data_type, is_nullable,
	        COALESCE(column_default, '')
	    FROM information_schema.columns WHERE table_schema = 'sage'
	     AND table_name = 'source_fix'
	     AND column_name IN ('origin', 'repo', 'pr_number', 'pr_state')
	    ORDER BY column_name`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name, typ, null, def string
		if err := rows.Scan(&name, &typ, &null, &def); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, name+" "+typ+" "+null+" "+def)
	}
	want := []string{"origin text NO 'agent'::text", "pr_number integer YES ",
		"pr_state text YES ", "repo text YES "}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("columns = %q, want %q", got, want)
	}
	insert := `INSERT INTO sage.source_fix (finding_id, packet_hash, stage, reported_by,
	    origin, pr_state) VALUES ($1, 'h', 'pr_opened', 'a', $2, $3)`
	if _, err := pool.Exec(ctx, insert, 1, "pg_sage", "open"); err != nil {
		t.Fatalf("insert a pg_sage row: %v", err)
	}
	for _, bad := range [][2]string{{"robot", "open"}, {"agent", "reopened"}} {
		if _, err := pool.Exec(ctx, insert, 2, bad[0], bad[1]); err == nil ||
			!strings.Contains(err.Error(), "check constraint") {
			t.Errorf("origin %q state %q: err = %v, want a check violation", bad[0], bad[1], err)
		}
	}
	var origin string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.source_fix (finding_id, packet_hash,
	    stage, reported_by) VALUES (3, 'h', 'pr_opened', 'a') RETURNING origin`).
		Scan(&origin); err != nil || origin != "agent" {
		t.Fatalf("default origin = %q (%v), want agent", origin, err)
	}
}

func TestSourceFixPROriginStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"ADD COLUMN IF NOT EXISTS origin", "SET LOCAL lock_timeout",
		"attname IN ('origin', 'repo', 'pr_number', 'pr_state')"} {
		if !strings.Contains(ddlSourceFixPROrigin, want) {
			t.Errorf("ddlSourceFixPROrigin lacks %q", want)
		}
	}
	steps := migrationSteps()
	if steps[len(steps)-1].name != "ddlSourceFixPROrigin" {
		t.Fatalf("last step = %s, want ddlSourceFixPROrigin", steps[len(steps)-1].name)
	}
}
