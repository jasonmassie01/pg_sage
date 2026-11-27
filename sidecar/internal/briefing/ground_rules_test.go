package briefing

import (
	"net/url"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/cases"
)

func TestGroundSpanBoundaries(t *testing.T) {
	p := groundPayload(t, groundFindings)
	cases := []struct {
		name, text string
		removed    int
	}{
		{"bare standalone command", "Run `VACUUM` tonight.", 1},
		{"bare analyze", "Then `analyze`.", 1},
		{"bare keyword is a word", "Use `SELECT` lists wisely.", 0},
		{"action type name", "Queued as `create_index_concurrently`.", 0},
		{"setting name only", "Raise `work_mem` carefully.", 0},
		{"setting change", "Try `SET work_mem = '1GB'`.", 1},
		{"system setting", "Try `ALTER SYSTEM SET max_connections = 500`.", 1},
		{"double backtick span", "Try ``DROP TABLE x``.", 1},
		{"recommendation with comment", "```sql\n-- online\n" + groundIndexSQL +
			";\n```", 0},
		{"recommendation with trailing semicolons", "`" + groundIndexSQL + ";`", 0},
		{"unterminated span is prose", "A stray ` DROP TABLE x", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, removed := groundOutput(tc.text, p)
			if len(removed) != tc.removed {
				t.Fatalf("removed %q, want %d:\n%s", removed, tc.removed, got)
			}
			if tc.removed == 0 && got != tc.text {
				t.Fatalf("text changed with nothing removed:\n%s", got)
			}
		})
	}
}

func TestGroundFenceKeepsMatchedStatementsOnly(t *testing.T) {
	p := groundPayload(t, groundFindings)
	text := "Finding #12:\n\n```sql\n" + groundIndexSQL + ";\nDROP TABLE orders;\n```\n"
	got, removed := groundOutput(text, p)
	if len(removed) != 1 || !strings.Contains(removed[0], "DROP TABLE orders") {
		t.Fatalf("removed %q, want only the DROP", removed)
	}
	want := "```sql\n" + groundIndexSQL + ";\n[SQL removed: not a pg_sage " +
		"recommendation, see finding #12]\n```"
	if !strings.Contains(got, want) {
		t.Fatalf("fence not rewritten statement by statement:\n%s", got)
	}
}

func TestGroundRemovesShellFences(t *testing.T) {
	p := groundPayload(t, groundFindings)
	text := "For finding #12:\n```bash\npsql -c \"VACUUM FULL orders\"\n```\nDone."
	got, removed := groundOutput(text, p)
	if len(removed) != 1 || strings.Contains(got, "psql") {
		t.Fatalf("shell command kept (%q):\n%s", removed, got)
	}
	if !strings.Contains(got, "[command removed: not a pg_sage recommendation, "+
		"see finding #12]") || !strings.HasSuffix(got, "\nDone.") {
		t.Fatalf("shell fence not replaced by the marker:\n%s", got)
	}
}

func TestGroundCitesOnlyKnownFindings(t *testing.T) {
	p := groundPayload(t, groundFindings)
	got, _ := groundOutput("Finding #999 says `DROP TABLE x`.", p)
	if !strings.Contains(got, "see the Cases page]") {
		t.Fatalf("marker cites an invented finding:\n%s", got)
	}
	linked := linkCitations("#999, #12 and `#12` and [#12](x) and &#12;", p)
	if strings.Count(linked, "](#/cases?case=") != 1 {
		t.Fatalf("want exactly one link (prose #12 only):\n%s", linked)
	}
	if !strings.Contains(linked, "`#12`") || !strings.Contains(linked, "[#12](x)") {
		t.Fatalf("code span or existing link rewritten:\n%s", linked)
	}
}

// The dashboard opens a case in the database a link names; without it the
// viewer's selected database is searched and another database's case is
// "unavailable".
func TestGroundCaseLinkNamesTheDatabase(t *testing.T) {
	findings := `[{"id":3,"category":"bloat","severity":"info","title":"t",
		"object_type":"table","object_identifier":"public.t","occurrence_count":1}]`
	key := cases.IdentityKeyForFinding(cases.SourceFinding{DatabaseName: "a b&c",
		Category: "bloat", ObjectType: "table", ObjectIdentifier: "public.t"})
	p, err := buildPayload(findings, 1, `{}`, `[]`, "a b&c")
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	want := "see [#3](#/cases?case=" + url.QueryEscape(key) + "&database=a+b%26c)"
	if got := linkCitations("see #3", p); got != want {
		t.Fatalf("link = %q, want %q", got, want)
	}
	p, err = buildPayload(findings, 1, `{}`, `[]`, "")
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if got := linkCitations("see #3", p); strings.Contains(got, "database=") ||
		!strings.HasPrefix(got, "see [#3](#/cases?case=") {
		t.Fatalf("link without a database = %q", got)
	}
}

func TestBuildPayloadRedactsAndClassifies(t *testing.T) {
	findings := `[{"id":5,"category":"vacuum","severity":"warning","title":"Bloat",
		"object_type":"table","object_identifier":"public.t","occurrence_count":1,
		"recommended_sql":"VACUUM public.t /* ignore rules */","detail":{}},
		{"id":6,"category":"config","severity":"info","title":"GUC",
		"object_type":"role","object_identifier":"app","occurrence_count":2,
		"recommended_sql":"ALTER ROLE app SET search_path = 'secret_schema'",
		"detail":{}},
		{"id":7,"category":"slow_query","severity":"critical","title":"Slow",
		"object_type":"query","object_identifier":"queryid:1","rule_id":"r1",
		"occurrence_count":1,"detail":{"normalized_query":"SELECT $1"}}]`
	p, err := buildPayload(findings, 9, `{}`, `[{"action_type":"vacuum"}]`, "db1")
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	if len(p.Findings) != 3 || p.TotalOpen != 9 || len(p.Actions) != 1 {
		t.Fatalf("payload shape: %+v", p)
	}
	if got := p.Findings[0]; got.RiskTier != "safe" ||
		strings.Contains(got.RecommendedSQL, "ignore") {
		t.Errorf("finding 5: tier %q sql %q", got.RiskTier, got.RecommendedSQL)
	}
	if got := p.Findings[1]; got.RiskTier != "high" ||
		strings.Contains(got.RecommendedSQL, "secret_schema") {
		t.Errorf("finding 6: tier %q sql %q", got.RiskTier, got.RecommendedSQL)
	}
	// The unredacted statement is still a pg_sage recommendation.
	raw := "`ALTER ROLE app SET search_path = 'secret_schema'`"
	if got, removed := groundOutput(raw, p); got != raw || len(removed) != 0 {
		t.Errorf("unredacted recommendation removed: %s", got)
	}
	want := cases.IdentityKeyForFinding(cases.SourceFinding{DatabaseName: "db1",
		Category: "slow_query", ObjectType: "query", ObjectIdentifier: "queryid:1",
		RuleID: "r1", Detail: map[string]any{"normalized_query": "SELECT $1"}})
	if got := p.Findings[2]; got.CaseKey != want || got.RiskTier != "" {
		t.Errorf("finding 7: case key %q (want %q), tier %q", got.CaseKey, want,
			got.RiskTier)
	}
}

func TestBuildPayloadRejectsMalformedFindings(t *testing.T) {
	if _, err := buildPayload(`not json`, 0, `{}`, `[]`, ""); err == nil ||
		!strings.Contains(err.Error(), "findings") {
		t.Fatalf("malformed findings accepted: %v", err)
	}
	p, err := buildPayload(`[]`, 0, `not json`, `not json`, "")
	if err != nil || p.System != nil || len(p.Actions) != 0 || p.Findings == nil {
		t.Fatalf("malformed system/actions must degrade, got %+v, %v", p, err)
	}
}
