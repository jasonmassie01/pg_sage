package briefing

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// seedGroundFinding inserts one open finding with a vetted recommendation
// and returns its id.
func seedGroundFinding(t *testing.T, pool *pgxpool.Pool, ctx context.Context) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation,
		recommended_sql, status, last_seen, occurrence_count)
		VALUES ('llm08_missing_index', 'critical', 'table', 'public.llm08_orders',
		        'Missing index for llm-08', '{}'::jsonb, 'Create it online.',
		        'CREATE INDEX CONCURRENTLY llm08_idx ON public.llm08_orders (c)',
		        'open', now(), 999998)
		RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.findings WHERE category = 'llm08_missing_index'`)
		_, _ = pool.Exec(bg, `DELETE FROM sage.briefings WHERE content_json::text
			LIKE '%llm08_missing_index%'`)
	})
	return id
}

// latestPayload reads the content_json and content_text of the newest
// briefing that mentions the seeded finding.
func latestPayload(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (Payload, string) {
	t.Helper()
	var raw []byte
	var text string
	err := pool.QueryRow(ctx, `SELECT content_json, content_text FROM sage.briefings
		WHERE content_json::text LIKE '%llm08_missing_index%'
		ORDER BY id DESC LIMIT 1`).Scan(&raw, &text)
	if err != nil {
		t.Fatalf("read stored briefing: %v", err)
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("content_json is not a payload: %v (%s)", err, raw)
	}
	return p, text
}

func payloadIDs(p Payload) []int64 {
	ids := make([]int64, 0, len(p.Findings))
	for _, f := range p.Findings {
		ids = append(ids, f.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// CHECK-08 (LLM off): the structured fallback stores the payload, not {}.
func TestGenerateStoresPayloadInContentJSON_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)
	id := seedGroundFinding(t, pool, ctx)
	out, err := New(pool, &config.Config{}, nil, noopLog).WithDatabaseName("db1").Generate(ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(out, "# pg_sage Health Briefing") ||
		!strings.Contains(out, "Missing index for llm-08") {
		t.Fatalf("structured fallback changed:\n%s", out)
	}
	p, text := latestPayload(t, pool, ctx)
	if text != out || !containsID(payloadIDs(p), id) || p.TotalOpen < 1 {
		t.Fatalf("content_json lacks finding %d (total %d): %+v", id, p.TotalOpen, p)
	}
	for _, f := range p.Findings {
		if f.ID == id && (f.RiskTier != "moderate" || f.CaseKey == "" ||
			!strings.HasPrefix(f.CaseKey, "finding:db1:llm08_missing_index:")) {
			t.Fatalf("finding %d stored as %+v", id, f)
		}
	}
}

// CHECK-08 (LLM on): content_json holds the same ids the model was sent, the
// stored text is grounded and linked, and the dispatched text is plain.
func TestGenerateGroundsLLMBriefing_LivePG(t *testing.T) {
	pool, ctx := requireDB(t)
	id := seedGroundFinding(t, pool, ctx)
	client, bodies := captureBriefingLLM(t, "Finding #"+jsonInt(id)+
		" is urgent.\n\n```sql\nVACUUM FULL public.llm08_orders;\n```\n")
	out, err := New(pool, &config.Config{}, client, noopLog).Generate(ctx)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if strings.Contains(out, "VACUUM FULL") || strings.Contains(out, "](#/cases") {
		t.Fatalf("dispatched briefing not grounded or not plain:\n%s", out)
	}
	p, text := latestPayload(t, pool, ctx)
	if strings.Contains(text, "VACUUM FULL") ||
		!strings.Contains(text, "[#"+jsonInt(id)+"](#/cases?case=") {
		t.Fatalf("stored briefing not grounded and linked:\n%s", text)
	}
	user := (*bodies)[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
	body := user[strings.Index(user, "\n")+1 : strings.LastIndex(user, "\n</data>")]
	var sent Payload
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("prompt data is not the payload: %v\n%s", err, body)
	}
	got, want := payloadIDs(p), payloadIDs(sent)
	if len(got) == 0 || jsonInts(got) != jsonInts(want) {
		t.Fatalf("content_json ids %v, prompt ids %v", got, want)
	}
	if !containsID(got, id) {
		t.Fatalf("seeded finding %d missing from %v", id, got)
	}
}

func jsonInt(v int64) string { b, _ := json.Marshal(v); return string(b) }

func jsonInts(v []int64) string { b, _ := json.Marshal(v); return string(b) }
