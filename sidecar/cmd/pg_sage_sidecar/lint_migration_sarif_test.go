package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: see lint_migration_test.go.

// requireSARIF checks the SARIF 2.1.0 schema's required members and enums
// on decoded output (no validator ships with the build): version, runs,
// tool.driver.name, each result's message.text, a level from the enum, a
// startLine >= 1, and a ruleId the driver declares.
func requireSARIF(t *testing.T, out string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("SARIF is not JSON: %v", err)
	}
	if doc["version"] != "2.1.0" || !strings.Contains(doc["$schema"].(string), "sarif-2.1.0") {
		t.Fatalf("version/schema %v %v", doc["version"], doc["$schema"])
	}
	run := doc["runs"].([]any)[0].(map[string]any)
	driver := run["tool"].(map[string]any)["driver"].(map[string]any)
	if driver["name"] != "pg_sage" {
		t.Fatalf("driver %v", driver)
	}
	declared := map[string]bool{}
	for _, r := range driver["rules"].([]any) {
		declared[r.(map[string]any)["id"].(string)] = true
	}
	for _, r := range run["results"].([]any) {
		res := r.(map[string]any)
		level := res["level"].(string)
		if level != "error" && level != "warning" && level != "note" {
			t.Fatalf("level %q is not in the SARIF enum", level)
		}
		if res["message"].(map[string]any)["text"].(string) == "" ||
			!declared[res["ruleId"].(string)] {
			t.Fatalf("result %v", res)
		}
		loc := res["locations"].([]any)[0].(map[string]any)["physicalLocation"].(map[string]any)
		if loc["region"].(map[string]any)["startLine"].(float64) < 1 ||
			loc["artifactLocation"].(map[string]any)["uri"].(string) == "" {
			t.Fatalf("location %v", loc)
		}
	}
	return doc
}

func TestLintMigrationCLISARIFFormat(t *testing.T) {
	file := writeMigration(t, "ALTER TABLE app.t ALTER COLUMN v TYPE bigint;\nCREATE FROBNICATE x;")
	code, out, _ := runLint(t, nil, "--static", "--format", "sarif", "--file", file)
	doc := requireSARIF(t, out)
	results := doc["runs"].([]any)[0].(map[string]any)["results"].([]any)
	if code != 2 || len(results) != 2 {
		t.Fatalf("exit %d, %d results", code, len(results))
	}
	if results[0].(map[string]any)["level"] != "error" ||
		results[1].(map[string]any)["level"] != "warning" {
		t.Fatalf("levels %v", results)
	}
	_, out, _ = runLint(t, nil, "--static", "--format", "sarif", "--file",
		writeMigration(t, "CREATE TABLE app.n (id int);"))
	doc = requireSARIF(t, out)
	if n := len(doc["runs"].([]any)[0].(map[string]any)["results"].([]any)); n != 0 {
		t.Fatalf("a safe migration has %d results", n)
	}
}

// Integration (real PostgreSQL): CREATE INDEX on a 1M-row table is risky
// (exit 2) with the CONCURRENTLY rewrite; varchar -> text is no rewrite.
func TestLintMigrationCLIAgainstDatabase(t *testing.T) {
	testdb.SkipUnlessLive(t)
	dsn := testdb.CreateDatabase(t, "lint_migration_cli")
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), `CREATE SCHEMA app;
		CREATE TABLE app.t (c int, v varchar(20));
		INSERT INTO app.t SELECT i, 'x' FROM generate_series(1, 1000000) i;
		ANALYZE app.t`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	env := map[string]string{lintDSNEnv: dsn}
	code, out, errOut := runLint(t, env, "--file", writeMigration(t, "CREATE INDEX x ON app.t(c);"))
	if code != 2 || !strings.Contains(out, "CREATE INDEX CONCURRENTLY x ON app.t(c);") {
		t.Fatalf("exit %d stdout %s stderr %s", code, out, errOut)
	}
	code, out, _ = runLint(t, env, "--file",
		writeMigration(t, "ALTER TABLE app.t ALTER COLUMN v TYPE text;"))
	if code == 3 || !strings.Contains(out, `"requires_rewrite": false`) {
		t.Fatalf("varchar -> text: exit %d %s", code, out)
	}
}
