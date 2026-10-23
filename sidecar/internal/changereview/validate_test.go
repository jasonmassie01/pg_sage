package changereview

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Request validation (CHECK-FND-06, pure part): every refusal is
// ErrInvalid with a message naming what is wrong.

func validRequest() Request {
	return Request{SQL: "ALTER TABLE orders ADD COLUMN note text;",
		PR: PRInfo{URL: "https://github.com/acme/shop/pull/7", Number: 7, Title: "Add note",
			Repo: "acme/shop", HeadSHA: "abc1234", BaseSHA: "def5678"}}
}

func TestValidateRequestAcceptsAndNormalizes(t *testing.T) {
	r := validRequest()
	r.PR.Title = "  " + strings.Repeat("t", 400) + "  "
	r.MigrationTool = "Rails"
	got, err := validateRequest(testConfig(), "testdb", cliCaller, r)
	if err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
	if got.Database != "testdb" {
		t.Fatalf("database = %q, want the service's", got.Database)
	}
	if n := len([]rune(got.PR.Title)); n != MaxPRTitleRunes {
		t.Fatalf("title has %d runes, want it cut to %d", n, MaxPRTitleRunes)
	}
	if got.MigrationTool != "rails" {
		t.Fatalf("migration tool %q, want lower-cased rails", got.MigrationTool)
	}
}

func TestValidateRequestRefusals(t *testing.T) {
	cfg := testConfig()
	cases := map[string]struct {
		edit func(*Request)
		want string
	}{
		"empty sql and diff": {func(r *Request) { r.SQL = "  \n" }, "sql or a diff"},
		"sql over the limit": {func(r *Request) { r.SQL = strings.Repeat("x", 100001) },
			"change_review.max_sql_bytes (100000)"},
		"files over the limit": {func(r *Request) {
			r.SQL = strings.Repeat("x", 60000)
			r.SQLFiles = []SQLFile{{Path: "a.sql", Content: strings.Repeat("y", 40001)}}
		}, "max_sql_bytes"},
		"diff over the limit": {func(r *Request) { r.Diff = strings.Repeat("d", 524289) },
			"change_review.max_diff_bytes (524288)"},
		"unknown tool":     {func(r *Request) { r.MigrationTool = "liquibasex" }, "liquibasex"},
		"other database":   {func(r *Request) { r.Database = "otherdb" }, "otherdb"},
		"http pr url":      {func(r *Request) { r.PR.URL = "http://github.com/a/b/pull/1" }, "https"},
		"pr url with user": {func(r *Request) { r.PR.URL = "https://u:p@github.com/a" }, "https"},
		"pr url space":     {func(r *Request) { r.PR.URL = "https://github.com/a b" }, "pr.url"},
		"bad head sha":     {func(r *Request) { r.PR.HeadSHA = "XYZ" }, "head_sha"},
		"bad base sha":     {func(r *Request) { r.PR.BaseSHA = "12" }, "base_sha"},
		"negative pr":      {func(r *Request) { r.PR.Number = -1 }, "pr.number"},
		"wait too long":    {func(r *Request) { r.WaitSeconds = 51 }, "wait_seconds"},
		"negative wait":    {func(r *Request) { r.WaitSeconds = -1 }, "wait_seconds"},
		"invalid utf8":     {func(r *Request) { r.SQL = "SELECT '\xff'" }, "UTF-8"},
		"nul byte":         {func(r *Request) { r.SQL = "SELECT 1\x00" }, "NUL"},
		"bad file path": {func(r *Request) {
			r.SQLFiles = []SQLFile{{Path: "a\nb.sql", Content: "SELECT 1"}}
		}, "path"},
		"long file path": {func(r *Request) {
			r.SQLFiles = []SQLFile{{Path: strings.Repeat("p", 513), Content: "SELECT 1"}}
		}, "path"},
		"too many files": {func(r *Request) {
			for i := 0; i <= MaxSQLFiles; i++ {
				r.SQLFiles = append(r.SQLFiles, SQLFile{Path: "f.sql", Content: "SELECT 1"})
			}
		}, "files"},
		"future opened_at": {func(r *Request) {
			r.PR.OpenedAt = ptr(time.Now().Add(48 * time.Hour))
		}, "opened_at"},
		"long repo": {func(r *Request) { r.PR.Repo = strings.Repeat("r", 301) }, "pr.repo"},
	}
	for name, c := range cases {
		r := validRequest()
		c.edit(&r)
		_, err := validateRequest(cfg, "testdb", cliCaller, r)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %q does not mention %q", name, err, c.want)
		}
	}
}

func TestValidateRequestBoundaries(t *testing.T) {
	cfg := testConfig()
	r := validRequest()
	r.SQL = strings.Repeat("x", 100000)
	r.WaitSeconds = MaxWaitSeconds
	if _, err := validateRequest(cfg, "testdb", cliCaller, r); err != nil {
		t.Fatalf("SQL at exactly max_sql_bytes and wait 50 refused: %v", err)
	}
	r = validRequest()
	r.SQL, r.Diff = "", strings.Repeat("d", 524288)
	if _, err := validateRequest(cfg, "testdb", cliCaller, r); err != nil {
		t.Fatalf("a diff at exactly max_diff_bytes refused: %v", err)
	}
	r = validRequest()
	r.PR = PRInfo{}
	if _, err := validateRequest(cfg, "testdb", cliCaller, r); err != nil {
		t.Fatalf("a request without PR metadata refused: %v", err)
	}
}

func TestValidateCaller(t *testing.T) {
	for name, c := range map[string]Caller{
		"empty actor":    {Actor: "", Source: SourceCLI},
		"star actor":     {Actor: "*", Source: SourceCLI},
		"long actor":     {Actor: strings.Repeat("a", 201), Source: SourceCLI},
		"unknown source": {Actor: "user:1", Source: "slack"},
	} {
		if _, err := validateRequest(testConfig(), "testdb", c, validRequest()); !errors.Is(err,
			ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestMigrationTools(t *testing.T) {
	cases := map[string]bool{"rails": true, "django": true, "alembic": true,
		"flyway": false, "": false, "sql": false}
	for name, transactional := range cases {
		tool, ok := LookupMigrationTool(name)
		if !ok {
			t.Fatalf("migration tool %q unknown", name)
		}
		if tool.Transactional != transactional {
			t.Errorf("%q transactional = %v, want %v", name, tool.Transactional,
				transactional)
		}
	}
	for _, name := range []string{"alembic", "django", "prisma"} {
		tool, _ := LookupMigrationTool(name)
		if !tool.ORM || tool.ExportCommand == "" {
			t.Errorf("%s: ORM %v export %q, want an ORM tool with an export command",
				name, tool.ORM, tool.ExportCommand)
		}
	}
	if _, ok := LookupMigrationTool("liquibasex"); ok {
		t.Fatal("an unknown tool was found")
	}
	names := MigrationToolNames()
	if len(names) < 10 || names[0] != "" {
		t.Fatalf("tool names = %v, want a sorted list starting with plain SQL", names)
	}
}

func TestResolveTransactional(t *testing.T) {
	in := newInput("id", "testdb", cliCaller, Request{SQL: "SELECT 1", MigrationTool: "rails"},
		config.DefaultChangeReviewConfig())
	if !in.Transactional || in.TransactionalFrom != TransactionalFromTool {
		t.Fatalf("rails: %v from %q, want true from the tool default", in.Transactional,
			in.TransactionalFrom)
	}
	in = newInput("id", "testdb", cliCaller, Request{SQL: "SELECT 1", MigrationTool: "rails",
		Transactional: ptr(false)}, config.DefaultChangeReviewConfig())
	if in.Transactional || in.TransactionalFrom != TransactionalFromRequest {
		t.Fatalf("explicit false: %v from %q", in.Transactional, in.TransactionalFrom)
	}
	in = newInput("id", "testdb", cliCaller, Request{SQL: "SELECT 1",
		SQLFiles: []SQLFile{{Path: "b.sql", Content: "SELECT 2"}}},
		config.DefaultChangeReviewConfig())
	if len(in.SQLFiles) != 2 || in.SQLFiles[0].Path != "" || in.SQLFiles[1].Path != "b.sql" {
		t.Fatalf("SQL files = %+v, want inline SQL first", in.SQLFiles)
	}
}
