//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// firstlook-02: the try-it and walkthrough pages start a stock postgres
// image, where pg_sage's role has no CREATE on the database. Run each page's
// role block as the superuser, connect with the DSN from the page's own
// pg_sage command, and require the schema bootstrap to succeed. Run it on
// PG 14 (PUBLIC still has CREATE on public) and PG 17.
//
// No concurrency or state-transition cases: each page is set up and
// bootstrapped once; bootstrap concurrency is covered in internal/schema.

var documentedRolePages = []string{
	"try-it-out.md", "walkthrough-linux.md", "walkthrough-windows.md",
}

var (
	pageCreatesRole = regexp.MustCompile(`(?i)CREATE\s+(USER|ROLE)\s+sage_agent\b`)
	pageAgentDSN    = regexp.MustCompile(`postgres(?:ql)?://sage_agent:[^@\s"']*@[^\s"']+`)
)

func TestDocumentedRoles(t *testing.T) {
	admin := adminDSN(t)
	if !pgAvailable(t, admin) {
		t.Fatal("PostgreSQL not reachable; the documented-roles e2e needs " +
			"SAGE_TEST_DATABASE_URL")
	}
	t.Logf("server_version_num=%d", serverVersionNum(t, admin))
	for _, page := range documentedRolePages {
		t.Run(page, func(t *testing.T) { bootstrapDocumentedRole(t, admin, page) })
	}
}

func bootstrapDocumentedRole(t *testing.T, admin, page string) {
	raw, err := os.ReadFile("../../docs/" + page)
	if err != nil {
		t.Fatalf("read docs/%s: %v", page, err)
	}
	doc := strings.ReplaceAll(string(raw), "\r\n", "\n")
	block := pageRoleBlock(t, page, doc)
	password := pagePassword(t, page, doc)

	pool, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(pool.Close)
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	name, role := "sage_docrole_"+suffix, "sage_agent_docrole_"+suffix
	testdb.LockAgentRolesOn(t, admin) // agent-named roles are cluster-wide
	t.Cleanup(func() {
		mustExecAdmin(t, pool, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	})
	createTestDB(t, pool, name)
	t.Cleanup(func() { dropTestDB(t, pool, name) })
	db, err := pgxpool.New(context.Background(), withDatabase(t, admin, name, nil))
	if err != nil {
		t.Fatalf("fixture pool: %v", err)
	}
	defer db.Close()
	mustExecAdmin(t, db, "CREATE SEQUENCE public.order_ids")
	for _, stmt := range blockStatements(t, page, strings.ReplaceAll(block, "sage_agent", role)) {
		mustExecAdmin(t, db, stmt)
	}

	agent, err := pgxpool.New(context.Background(),
		withDatabase(t, admin, name, url.UserPassword(role, password)))
	if err != nil {
		t.Fatalf("agent pool: %v", err)
	}
	defer agent.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := schema.Bootstrap(ctx, agent); err != nil {
		t.Fatalf("docs/%s: bootstrap as the documented role failed: %v", page, err)
	}
	assertDocumentedRolePosture(t, db, page, role)
}

// pageRoleBlock returns the first ```sql block on the page that creates
// sage_agent.
func pageRoleBlock(t *testing.T, page, doc string) string {
	t.Helper()
	for rest := doc; ; {
		_, after, found := strings.Cut(rest, "```sql\n")
		if !found {
			t.Fatalf("docs/%s has no ```sql block that creates sage_agent", page)
		}
		body, tail, closed := strings.Cut(after, "```")
		if !closed {
			t.Fatalf("docs/%s has an unclosed ```sql block", page)
		}
		if pageCreatesRole.MatchString(body) {
			return body
		}
		rest = tail
	}
}

// pagePassword parses the page's pg_sage DSN, not the role block, so a
// mismatch between the two fails authentication here as it would for a user.
func pagePassword(t *testing.T, page, doc string) string {
	t.Helper()
	dsns := pageAgentDSN.FindAllString(doc, -1)
	if len(dsns) != 1 {
		t.Fatalf("docs/%s has %d sage_agent DSNs, want 1: %q", page, len(dsns), dsns)
	}
	u, err := url.Parse(dsns[0])
	if err != nil {
		t.Fatalf("docs/%s DSN %q does not parse: %v", page, dsns[0], err)
	}
	password, ok := u.User.Password()
	if !ok || password == "" {
		t.Fatalf("docs/%s DSN %q has no password", page, dsns[0])
	}
	// The page runs its block in the postgres database; the DSN must target it.
	if u.Path != "/postgres" {
		t.Fatalf("docs/%s DSN targets %q, not the database the role block ran in",
			page, u.Path)
	}
	return password
}

func blockStatements(t *testing.T, page, block string) []string {
	t.Helper()
	var statements []string
	for _, stmt := range strings.Split(stripSQLComments(block), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			statements = append(statements, stmt)
		}
	}
	if len(statements) < 3 {
		t.Fatalf("docs/%s role block parsed into %d statements: %q",
			page, len(statements), statements)
	}
	return statements
}

// assertDocumentedRolePosture checks the bootstrap ran without database
// CREATE and that the role stays read-only until Grant more.
func assertDocumentedRolePosture(t *testing.T, db *pgxpool.Pool, page, role string) {
	t.Helper()
	var owner string
	var config, dbCreate, signal, monitor, sequence bool
	err := db.QueryRow(context.Background(), `
SELECT (SELECT nspowner::regrole::text FROM pg_namespace WHERE nspname = 'sage'),
       to_regclass('sage.config') IS NOT NULL,
       has_database_privilege($1, current_database(), 'CREATE'),
       pg_has_role($1, 'pg_signal_backend', 'MEMBER'),
       pg_has_role($1, 'pg_monitor', 'MEMBER'),
       has_sequence_privilege($1, 'public.order_ids', 'SELECT')`, role).
		Scan(&owner, &config, &dbCreate, &signal, &monitor, &sequence)
	if err != nil {
		t.Fatalf("docs/%s posture query: %v", page, err)
	}
	want := map[string][2]bool{
		"sage.config exists":         {config, true},
		"CREATE on the database":     {dbCreate, false},
		"pg_signal_backend up front": {signal, false},
		"pg_monitor":                 {monitor, true},
		"SELECT on public sequences": {sequence, true},
	}
	for label, got := range want {
		if got[0] != got[1] {
			t.Errorf("docs/%s role: %s = %v, want %v", page, label, got[0], got[1])
		}
	}
	if owner != role {
		t.Errorf("docs/%s: sage schema owner = %q, want %q", page, owner, role)
	}
}

func serverVersionNum(t *testing.T, dsn string) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect for server version: %v", err)
	}
	defer conn.Close(context.Background())
	var v int
	if err := conn.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}
	return v
}
