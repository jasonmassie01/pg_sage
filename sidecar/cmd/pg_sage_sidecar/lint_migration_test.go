package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/migration"
)

// No concurrent access tests: each run reviews one file with its own
// connection; Review's parallel use is covered in the migration package.

func writeMigration(t *testing.T, sql string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "0001_change.sql")
	if err := os.WriteFile(path, []byte(sql), 0o600); err != nil {
		t.Fatalf("write migration: %v", err)
	}
	return path
}

func runLint(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runLintMigration(context.Background(), args, func(k string) string {
		return env[k]
	}, &out, &errOut)
	return code, out.String(), errOut.String()
}

// CHECK-23: the exit-code table, and --static with no
// SAGE_LINT_DATABASE_URL.
func TestLintMigrationCLIExitCodes(t *testing.T) {
	big := strings.Repeat("-- pad\n", migration.MaxReviewBytes/7+1)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"safe", []string{"--static", "--file", writeMigration(t,
			"CREATE TABLE app.n (id int);")}, 0},
		{"review: static index build", []string{"--static", "--file",
			writeMigration(t, "CREATE INDEX x ON app.t (c);")}, 1},
		{"review: unparseable", []string{"--static", "--file",
			writeMigration(t, "CREATE FROBNICATE x;")}, 1},
		{"risky: rewrite under ACCESS EXCLUSIVE", []string{"--static", "--pg-version", "16",
			"--file", writeMigration(t, "ALTER TABLE app.t ALTER COLUMN v TYPE bigint;")}, 2},
		{"tool error: empty file", []string{"--static", "--file", writeMigration(t, "")}, 3},
		{"tool error: over 100 kB", []string{"--static", "--file", writeMigration(t, big)}, 3},
		{"tool error: missing file", []string{"--static", "--file",
			filepath.Join(t.TempDir(), "nope.sql")}, 3},
		{"tool error: no --file", []string{"--static"}, 3},
		{"tool error: bad format", []string{"--static", "--format", "xml", "--file",
			writeMigration(t, "SELECT 1;")}, 3},
		{"tool error: bad version", []string{"--static", "--pg-version", "12", "--file",
			writeMigration(t, "SELECT 1;")}, 3},
		{"tool error: no database and not static", []string{"--file",
			writeMigration(t, "SELECT 1;")}, 3},
		{"tool error: stray argument", []string{"--static", "--file",
			writeMigration(t, "SELECT 1;"), "extra"}, 3},
	}
	for _, tc := range cases {
		code, out, errOut := runLint(t, nil, tc.args...)
		if code != tc.want {
			t.Errorf("%s: exit %d, want %d (stdout %q stderr %q)", tc.name, code, tc.want,
				out, errOut)
		}
		if tc.want == 3 && (errOut == "" || out != "") {
			t.Errorf("%s: a tool error must explain itself on stderr only: %q / %q",
				tc.name, out, errOut)
		}
	}
}

// Error propagation: an unreachable database is exit 3 naming the
// connection, never a verdict.
func TestLintMigrationCLIConnectionFailure(t *testing.T) {
	env := map[string]string{lintDSNEnv: "postgres://u:secretpw@127.0.0.1:1/db?" +
		"connect_timeout=2&sslmode=disable"}
	code, out, errOut := runLint(t, env, "--file", writeMigration(t, "CREATE TABLE n (a int);"))
	if code != 3 || out != "" || !strings.Contains(errOut, "connect to "+lintDSNEnv) {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
	if strings.Contains(errOut, "secretpw") {
		t.Fatalf("the password reached stderr: %q", errOut)
	}
	env[lintDSNEnv] = "::not a dsn::"
	code, _, errOut = runLint(t, env, "--file", writeMigration(t, "SELECT 1;"))
	if code != 3 || !strings.Contains(errOut, "not valid") {
		t.Fatalf("bad DSN: exit %d stderr %q", code, errOut)
	}
}

func TestLintMigrationCLIJSON(t *testing.T) {
	file := writeMigration(t, "SET lock_timeout = '5s';\nALTER TABLE app.t DROP COLUMN b;")
	code, out, _ := runLint(t, nil, "--static", "--file", file)
	var got struct {
		File string `json:"file"`
		migration.Report
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if code != 1 || got.File != file || got.Verdict != "review" || !got.Static ||
		len(got.Statements) != 2 || got.Statements[1].Line != 2 ||
		got.Statements[1].Hazards[0].Size != "unknown" {
		t.Fatalf("exit %d report %+v", code, got)
	}
}

// reGitHubCommand is a workflow command line: level, properties, message,
// with no raw newline in the message.
var reGitHubCommand = regexp.MustCompile(
	`^::(error|warning|notice) file=[^,:]+(,line=[1-9][0-9]*)?(,title=[^,:]+)?::[^\r\n]+$`)

// Integration (format): every GitHub line is a valid workflow command,
// risky maps to error, and the message keeps the rewrite (escaped).
func TestLintMigrationCLIGitHubFormat(t *testing.T) {
	file := writeMigration(t, "CREATE TABLE app.n (id int);\n\n"+
		"ALTER TABLE app.t ALTER COLUMN v TYPE bigint;\nCREATE FROBNICATE x;")
	code, out, _ := runLint(t, nil, "--static", "--format", "github", "--file", file)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 2 || len(lines) < 3 {
		t.Fatalf("exit %d output %q", code, out)
	}
	for _, l := range lines {
		if !reGitHubCommand.MatchString(l) {
			t.Errorf("not a workflow command: %q", l)
		}
	}
	if !strings.HasPrefix(lines[0], "::error file=") || !strings.Contains(lines[0], ",line=3,") ||
		!strings.Contains(lines[0], "%0ASET lock_timeout") {
		t.Fatalf("first annotation %q, want the risky rewrite at line 3", lines[0])
	}
	if !strings.Contains(out, "::warning file=") || !strings.Contains(out, ",line=4,") {
		t.Fatalf("the unrecognized statement is not a warning at line 4: %q", out)
	}
	if got := githubEscape("a:b,c%\n", true); got != "a%3Ab%2Cc%25%0A" {
		t.Fatalf("escape %q", got)
	}
}

// MCP lint_migration applies migration.ddl_row_threshold, as the advisor
// does (migration-11); unset stays 0, which the review reads as 10000.
func TestAgentToolOptionsCarryTheRowThreshold(t *testing.T) {
	c := &config.Config{}
	c.Migration.DDLRowThreshold = 777
	if got := agentToolOptions(c).DDLRowThreshold; got != 777 {
		t.Fatalf("DDLRowThreshold = %d, want 777", got)
	}
	if got := agentToolOptions(nil).DDLRowThreshold; got != 0 {
		t.Fatalf("no config: DDLRowThreshold = %d, want 0 (the default)", got)
	}
}
