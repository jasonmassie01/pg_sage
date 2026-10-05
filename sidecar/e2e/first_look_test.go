//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Five-minute time to value (roadmap phase 3): start the sidecar exactly
// as docs/quickstart.md shows, against a fresh database with three seeded
// problems, and require a non-empty catalog-only first look within 60 s of
// the process start, with the time to first finding exported as a metric.
const firstLookDeadline = 60 * time.Second

type firstLookReport struct {
	Databases []struct {
		Database string `json:"database"`
		Report   *struct {
			Items []struct {
				Rule     string `json:"rule"`
				Object   string `json:"object"`
				Evidence []struct {
					Ref string `json:"ref"`
				} `json:"evidence"`
			} `json:"items"`
		} `json:"report"`
	} `json:"databases"`
}

func TestFirstLookWithinAMinute(t *testing.T) {
	admin := adminDSN(t)
	if !pgAvailable(t, admin) {
		t.Fatal("PostgreSQL not reachable; the first-look e2e needs SAGE_TEST_DATABASE_URL")
	}
	requireDefaultPortsFree(t)
	binary := buildBinary(t)
	dsn, name := seededQuickstartDatabase(t, admin)

	start := time.Now()
	env := startDocumented(t, binary, nil, []string{"SAGE_DATABASE_URL=" + dsn})
	report := waitForFirstLook(t, env, name, start)
	elapsed := time.Since(start)
	rules := map[string]bool{}
	for _, it := range report.Databases[0].Report.Items {
		rules[it.Rule] = true
		if len(it.Evidence) == 0 || it.Evidence[0].Ref == "" {
			t.Errorf("item %s %s cites no catalog evidence", it.Rule, it.Object)
		}
	}
	for _, want := range []string{"duplicate_index", "unindexed_foreign_key",
		"sequence_runway"} {
		if !rules[want] {
			t.Errorf("first look lacks %s: %v", want, rules)
		}
	}
	t.Logf("first look ready %s after process start", elapsed.Round(time.Millisecond))

	ttff := ttffMetric(t, env, name)
	if ttff <= 0 || ttff > firstLookDeadline.Seconds() {
		t.Fatalf("pg_sage_time_to_first_finding_seconds = %v, want (0, 60]", ttff)
	}
	t.Logf("TTFF_SECONDS=%.3f", ttff)
	assertReadOnlyOnboarding(t, env, dsn, name)
}

// seededQuickstartDatabase creates a database with a duplicate index, an
// unindexed foreign key and a sequence near its limit, then runs the role
// SQL from docs/quickstart.md. It returns the DSN for that role and the
// database name.
func seededQuickstartDatabase(t *testing.T, admin string) (string, string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(pool.Close)
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	name, role := "sage_ttv_"+suffix, "sage_ttv_agent_"+suffix
	password := randomHex(t)
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
	for _, stmt := range []string{
		"CREATE TABLE public.customers (id int PRIMARY KEY)",
		"INSERT INTO public.customers SELECT g FROM generate_series(1, 100) g",
		"CREATE TABLE public.orders (id serial PRIMARY KEY, customer_id int " +
			"REFERENCES public.customers (id), total int)",
		"INSERT INTO public.orders (customer_id, total) SELECT 1 + g % 100, g " +
			"FROM generate_series(1, 5000) g",
		"CREATE INDEX orders_total_a ON public.orders (total)",
		"CREATE INDEX orders_total_b ON public.orders (total)",
		"SELECT setval('public.orders_id_seq', 2000000000)",
		"ANALYZE public.orders",
	} {
		mustExecAdmin(t, db, stmt)
	}
	for _, stmt := range quickstartRoleSQL(t, role, password) {
		mustExecAdmin(t, db, stmt)
	}
	return withDatabase(t, admin, name, url.UserPassword(role, password)), name
}

// quickstartRoleSQL extracts the first sql block under the role heading of
// docs/quickstart.md, substituting the role and password.
func quickstartRoleSQL(t *testing.T, role, password string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/quickstart.md")
	if err != nil {
		t.Fatalf("read quickstart: %v", err)
	}
	_, section, found := strings.Cut(string(raw), "## 1. Create a role for pg_sage")
	_, block, fenced := strings.Cut(section, "```sql")
	block, _, closed := strings.Cut(block, "```")
	if !found || !fenced || !closed {
		t.Fatal("docs/quickstart.md lost its role sql block")
	}
	block = strings.NewReplacer("sage_agent", role, "YOUR_PASSWORD", password).Replace(block)
	var statements []string
	for _, stmt := range strings.Split(stripSQLComments(block), ";") {
		if stmt = strings.TrimSpace(stmt); stmt != "" {
			statements = append(statements, stmt)
		}
	}
	if len(statements) < 3 {
		t.Fatalf("quickstart role SQL parsed into %d statements", len(statements))
	}
	return statements
}

func waitForFirstLook(t *testing.T, env *testEnv, name string,
	start time.Time) firstLookReport {
	t.Helper()
	target := env.apiBase + "/api/v1/first-look?database=" + url.QueryEscape(name)
	for time.Since(start) < firstLookDeadline {
		code, body := httpGet(t, env, target)
		var r firstLookReport
		if code == 200 && json.Unmarshal([]byte(body), &r) == nil &&
			len(r.Databases) == 1 && r.Databases[0].Report != nil &&
			len(r.Databases[0].Report.Items) > 0 {
			return r
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no non-empty first look within %s of start\nstderr:\n%s", firstLookDeadline,
		env.stderr.String())
	return firstLookReport{}
}

func ttffMetric(t *testing.T, env *testEnv, name string) float64 {
	t.Helper()
	re := regexp.MustCompile(`pg_sage_time_to_first_finding_seconds\{database="` +
		regexp.QuoteMeta(name) + `"\} ([0-9.eE+-]+)`)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		code, body := httpGet(t, env, env.promBase+"/metrics")
		if m := re.FindStringSubmatch(body); code == 200 && m != nil {
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				t.Fatalf("parse ttff %q: %v", m[1], err)
			}
			return v
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("pg_sage_time_to_first_finding_seconds never appeared on /metrics")
	return 0
}

// assertReadOnlyOnboarding checks the checklist reports a new, read-only
// install and that nothing changed outside pg_sage's own schema.
func assertReadOnlyOnboarding(t *testing.T, env *testEnv, dsn, name string) {
	t.Helper()
	code, body := httpGet(t, env, env.apiBase+"/api/v1/onboarding?database="+
		url.QueryEscape(name))
	assertStatusOK(t, "onboarding", code)
	for _, want := range []string{`"install_kind":"new"`, `"trust_level":"observation"`,
		`"id":"grant_more"`, `"id":"first_look"`} {
		assertContains(t, "onboarding", body, want)
	}
	code, body = httpGet(t, env, env.apiBase+"/api/v1/onboarding/trust?database="+
		url.QueryEscape(name))
	assertStatusOK(t, "trust guide", code)
	assertContains(t, "trust guide", body, `"key":"trust.level"`)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("monitored pool: %v", err)
	}
	defer pool.Close()
	var actions, indexes int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.action_log").Scan(&actions); err != nil || actions != 0 {
		t.Fatalf("action_log rows = %d err %v: a new install must only observe", actions, err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_indexes
		WHERE schemaname = 'public'`).Scan(&indexes); err != nil || indexes != 4 {
		t.Fatalf("public indexes = %d err %v, want the 4 seeded (nothing dropped/added)",
			indexes, err)
	}
}
