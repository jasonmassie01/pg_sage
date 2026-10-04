package ask

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/facts"
)

// The read-only tools against a real sage schema: every result is
// citable evidence with a typed id (finding:42, action:7, ...), a digest
// of the exact text the model saw and a short label. Invalid arguments
// are refused before anything is read; a missing object is a citable
// "not_found" (absence is a valid answer).

func runTool(t *testing.T, s *Service, c Caller, name, args string) (agentloop.Output, error) {
	t.Helper()
	for _, tool := range s.newSession(c).tools() {
		if tool.Name == name {
			return tool.Run(context.Background(), json.RawMessage(args))
		}
	}
	t.Fatalf("tool %s is not offered to %+v", name, c)
	return agentloop.Output{}, nil
}

func mustTool(t *testing.T, s *Service, c Caller, name, args string) agentloop.Output {
	t.Helper()
	out, err := runTool(t, s, c, name, args)
	if err != nil {
		t.Fatalf("%s %s: %v", name, args, err)
	}
	if out.Evidence == nil {
		t.Fatalf("%s %s: no evidence", name, args)
	}
	if out.Evidence.Digest != digestOf(out.Text) || out.Evidence.Text != out.Text {
		t.Fatalf("%s: evidence digest/text do not match the text shown", name)
	}
	return out
}

func wantInvalid(t *testing.T, s *Service, c Caller, name, args string) {
	t.Helper()
	if _, err := runTool(t, s, c, name, args); !errors.Is(err, agentloop.ErrInvalidArgs) {
		t.Errorf("%s %s: err = %v, want ErrInvalidArgs", name, args, err)
	}
}

func TestTool_ListFindings(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	crit := f.finding(findingSeed{Severity: "critical", Object: "public.orders",
		Title: "Missing index on public.orders(customer_id)"})
	warn := f.finding(findingSeed{Category: "table_bloat", Object: "public.events",
		Title: "Bloated table public.events"})
	f.finding(findingSeed{Object: "public.old", Title: "Resolved thing", Status: "resolved"})

	out := mustTool(t, s, viewer, "list_findings", `{}`)
	if out.Evidence.ID != "findings:open" || !strings.HasPrefix(out.Evidence.Label,
		"findings:open") {
		t.Fatalf("evidence = %+v", out.Evidence)
	}
	for _, want := range []string{"Missing index on public.orders(customer_id)",
		"Bloated table public.events", itoa(crit), itoa(warn), "critical"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, out.Text)
		}
	}
	if strings.Contains(out.Text, "Resolved thing") {
		t.Fatal("a resolved finding is listed as open")
	}
	critOnly := mustTool(t, s, viewer, "list_findings", `{"severity":"critical","limit":5}`)
	if critOnly.Evidence.ID != "findings:open:critical" ||
		strings.Contains(critOnly.Text, "Bloated") {
		t.Fatalf("critical filter: %s\n%s", critOnly.Evidence.ID, critOnly.Text)
	}
	all := mustTool(t, s, viewer, "list_findings", `{"status":"all"}`)
	if !strings.Contains(all.Text, "Resolved thing") || all.Evidence.ID != "findings:all" {
		t.Fatalf("all: %s", all.Text)
	}
	for _, bad := range []string{`{"status":"bogus"}`, `{"limit":0}`, `{"limit":21}`,
		`{"severity":"apocalyptic"}`, `{"unknown":1}`, `[]`, `{"category":"` +
			strings.Repeat("c", 101) + `"}`} {
		wantInvalid(t, s, viewer, "list_findings", bad)
	}
}

func TestTool_ListFindingsEmptyIsCitableAbsence(t *testing.T) {
	f := newFixture(t)
	out := mustTool(t, f.service(f.deps(nil)), viewer, "list_findings", `{}`)
	if out.Status != "empty" || !strings.Contains(out.Text, "no findings") {
		t.Fatalf("empty list = %+v", out)
	}
}

func TestTool_GetFinding(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	id := f.finding(findingSeed{Object: "public.orders", Title: "Missing index",
		Recommendation: "Create the index",
		SQL: "CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders " +
			"(customer_id)",
		Rollback: "DROP INDEX CONCURRENTLY public.idx_orders_customer",
		Detail:   map[string]any{"seq_scans": 4200, "rows": 1250000}})
	q := f.queued(id, "CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders "+
		"(customer_id)", "pending")
	out := mustTool(t, s, viewer, "get_finding", `{"id":`+itoa(id)+`}`)
	if out.Evidence.ID != "finding:"+itoa(id) {
		t.Fatalf("id = %s", out.Evidence.ID)
	}
	for _, want := range []string{"Missing index", "CREATE INDEX CONCURRENTLY",
		"DROP INDEX CONCURRENTLY public.idx_orders_customer", "4200", "1250000",
		"pending approval " + itoa(q)} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, out.Text)
		}
	}
	missing := mustTool(t, s, viewer, "get_finding", `{"id":999999}`)
	if missing.Status != "not_found" || missing.Evidence.ID != "finding:999999" {
		t.Fatalf("missing finding = %+v", missing)
	}
	for _, bad := range []string{`{}`, `{"id":0}`, `{"id":-3}`, `{"id":"x"}`} {
		wantInvalid(t, s, viewer, "get_finding", bad)
	}
}

func TestTool_ActionsWithVerificationOutcomes(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	fid := f.finding(findingSeed{Object: "public.orders", Title: "Missing index"})
	aid := f.action(fid, "CREATE INDEX CONCURRENTLY idx_orders_customer ON public.orders "+
		"(customer_id)", "improved", "met", map[string]any{"mean_ms_change_pct": -40},
		map[string]any{"mean_ms_change_pct": -35.5})
	pending := f.action(fid, "ANALYZE public.orders", "", "", nil, nil)

	list := mustTool(t, s, viewer, "list_actions", `{}`)
	if list.Evidence.ID != "actions:recent" || !strings.Contains(list.Text, itoa(aid)) ||
		!strings.Contains(list.Text, "improved") || !strings.Contains(list.Text, itoa(pending)) {
		t.Fatalf("list = %s", list.Text)
	}
	one := mustTool(t, s, viewer, "get_action", `{"id":`+itoa(aid)+`}`)
	for _, want := range []string{"improved", "met", "-40", "-35.5", "approved by user 42",
		"DROP INDEX CONCURRENTLY public.idx_orders_customer"} {
		if !strings.Contains(one.Text, want) {
			t.Errorf("action text lacks %q:\n%s", want, one.Text)
		}
	}
	unverified := mustTool(t, s, viewer, "get_action", `{"id":`+itoa(pending)+`}`)
	if !strings.Contains(unverified.Text, "no verification outcome") {
		t.Fatalf("an action without an outcome: %s", unverified.Text)
	}
	if nf := mustTool(t, s, viewer, "get_action", `{"id":424242}`); nf.Status != "not_found" {
		t.Fatalf("missing action = %+v", nf)
	}
	wantInvalid(t, s, viewer, "list_actions", `{"limit":100}`)
}

func TestTool_ListApprovalsOnlyPending(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	fid := f.finding(findingSeed{Object: "public.orders", Title: "Missing index"})
	p := f.queued(fid, "CREATE INDEX CONCURRENTLY a ON public.orders (x)", "pending")
	r := f.queued(fid, "CREATE INDEX CONCURRENTLY b ON public.orders (y)", "rejected")
	out := mustTool(t, s, viewer, "list_approvals", `{}`)
	if out.Evidence.ID != "approvals:pending" || !strings.Contains(out.Text, "queue "+itoa(p)) ||
		strings.Contains(out.Text, "queue "+itoa(r)) {
		t.Fatalf("approvals = %s", out.Text)
	}
}

func TestTool_ListFacts(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	_, _, err := facts.NewStore(f.pool).Propose(f.ctx, facts.Proposal{
		Type: facts.TypeTestFixture, Kind: facts.KindSchema, Subject: "test_tenant_*",
		Source: facts.SourceDetector, Evidence: []facts.Citation{{Kind: "catalog",
			Ref: "schemas:test_tenant_*", Detail: "3 idle copies"}}})
	if err != nil {
		t.Fatal(err)
	}
	out := mustTool(t, s, viewer, "list_facts", `{"status":"proposed"}`)
	if out.Evidence.ID != "facts:proposed" || !strings.Contains(out.Text, "test_tenant_*") ||
		!strings.Contains(out.Text, "proposed") {
		t.Fatalf("facts = %+v", out)
	}
	if none := mustTool(t, s, viewer, "list_facts", `{"status":"confirmed"}`); none.Status !=
		"empty" {
		t.Fatalf("confirmed facts = %+v", none)
	}
	wantInvalid(t, s, viewer, "list_facts", `{"status":"maybe"}`)
}

func TestTool_ListIncidents(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	active := f.incident("critical", "lock chain behind pid 4242", false)
	f.incident("warning", "old resolved spike", true)
	out := mustTool(t, s, viewer, "list_incidents", `{}`)
	if out.Evidence.ID != "incidents:active" || !strings.Contains(out.Text, active) ||
		!strings.Contains(out.Text, "pid 4242") || strings.Contains(out.Text, "old resolved") {
		t.Fatalf("active incidents = %s", out.Text)
	}
	all := mustTool(t, s, viewer, "list_incidents", `{"status":"all"}`)
	if all.Evidence.ID != "incidents:all" || !strings.Contains(all.Text, "old resolved") {
		t.Fatalf("all incidents = %s", all.Text)
	}
}

func TestTool_DescribeTable(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	f.exec(`DROP TABLE IF EXISTS public.ask_orders`)
	f.exec(`CREATE TABLE public.ask_orders (id bigint PRIMARY KEY, customer_id int, note text)`)
	t.Cleanup(func() { f.exec(`DROP TABLE IF EXISTS public.ask_orders`) })
	f.exec(`CREATE INDEX ask_orders_customer ON public.ask_orders (customer_id)`)
	f.exec(`COMMENT ON TABLE public.ask_orders IS 'Customer orders, one row per checkout'`)
	out := mustTool(t, s, viewer, "describe_table", `{"table":"public.ask_orders"}`)
	for _, want := range []string{"Customer orders, one row per checkout", "customer_id",
		"ask_orders_customer", "3 columns"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, out.Text)
		}
	}
	if out.Evidence.ID != "table:public.ask_orders" {
		t.Fatalf("id = %s", out.Evidence.ID)
	}
	if nf := mustTool(t, s, viewer, "describe_table", `{"table":"public.nope_nope"}`); nf.Status !=
		"not_found" {
		t.Fatalf("missing table = %+v", nf)
	}
	for _, bad := range []string{`{"table":"ask_orders"}`, `{"table":"public.x;drop table y"}`,
		`{"table":"pg_catalog.pg_class"}`, `{"table":"information_schema.tables"}`,
		`{"table":""}`, `{}`} {
		wantInvalid(t, s, viewer, "describe_table", bad)
	}
}

func TestTool_ExplainConfigNeverShowsSecrets(t *testing.T) {
	f := newFixture(t)
	d := f.deps(nil)
	d.Settings.LLM.APIKey = "sk-top-secret"
	d.Settings.Ask.RetentionDays = 14
	s := f.service(d)
	out := mustTool(t, s, viewer, "explain_config", `{"key":"ask.retention_days"}`)
	if out.Evidence.ID != "config:ask.retention_days" || !strings.Contains(out.Text, "14") ||
		!strings.Contains(out.Text, "restart") {
		t.Fatalf("config = %s", out.Text)
	}
	secret := mustTool(t, s, viewer, "explain_config", `{"key":"llm.api_key"}`)
	if strings.Contains(secret.Text, "sk-top-secret") || !strings.Contains(secret.Text,
		"not shown") {
		t.Fatalf("secret = %s", secret.Text)
	}
	if nf := mustTool(t, s, viewer, "explain_config", `{"key":"no.such_key"}`); nf.Status !=
		"not_found" {
		t.Fatalf("unknown key = %+v", nf)
	}
	for _, bad := range []string{`{"key":"ASK; DROP"}`, `{"key":""}`, `{}`} {
		wantInvalid(t, s, viewer, "explain_config", bad)
	}
}

func TestTool_ExplainConcept(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	for _, topic := range ConceptTopics() {
		out := mustTool(t, s, viewer, "explain_concept", `{"topic":"`+topic+`"}`)
		if out.Evidence.ID != "doc:"+topic || len(out.Text) < 80 {
			t.Errorf("%s = %+v", topic, out)
		}
	}
	trust := mustTool(t, s, viewer, "explain_concept", `{"topic":"ask_sage"}`)
	if !strings.Contains(trust.Text, "never executes") {
		t.Fatalf("ask_sage concept does not state the limit: %s", trust.Text)
	}
	wantInvalid(t, s, viewer, "explain_concept", `{"topic":"astrology"}`)
}

func TestTool_InvestigationsAndTrustThroughSources(t *testing.T) {
	f := newFixture(t)
	d := f.deps(nil)
	inv := &fakeInvestigations{list: `[{"id":"6f1c","state":"concluded","root":"lock_chain"}]`,
		detail: map[string]string{"6f1c": `{"id":"6f1c","root":"lock_chain","probe_count":4}`}}
	d.Investigations = inv
	d.Trust = fakeTrust(`{"rows":[{"family":"index","class":"create","level":2}]}`)
	s := f.service(d)
	list := mustTool(t, s, viewer, "list_investigations", `{}`)
	if list.Evidence.ID != "investigations:recent" || !strings.Contains(list.Text, "6f1c") {
		t.Fatalf("list = %+v", list)
	}
	one := mustTool(t, s, viewer, "get_investigation", `{"id":"6f1c"}`)
	if one.Evidence.ID != "investigation:6f1c" || !strings.Contains(one.Text, `"probe_count":4`) {
		t.Fatalf("detail = %+v", one)
	}
	if nf := mustTool(t, s, viewer, "get_investigation", `{"id":"0000"}`); nf.Status !=
		"not_found" {
		t.Fatalf("missing investigation = %+v", nf)
	}
	trust := mustTool(t, s, viewer, "trust_ledger", `{}`)
	if trust.Evidence.ID != "trust:testdb" || !strings.Contains(trust.Text, `"level":2`) {
		t.Fatalf("trust = %+v", trust)
	}
	inv.err = errors.New("store down")
	if _, err := runTool(t, s, viewer, "list_investigations", `{}`); err == nil ||
		errors.Is(err, agentloop.ErrInvalidArgs) || !strings.Contains(err.Error(), "store down") {
		t.Fatalf("a source failure must be a tool error naming its cause: %v", err)
	}
}

func TestTool_SourcesAbsentMeansToolsAbsent(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(nil))
	names := toolNames(s.newSession(operator).tools())
	for _, absent := range []string{"list_investigations", "get_investigation",
		"trust_ledger", "top_queries", "open_investigation", "propose_action"} {
		if names[absent] {
			t.Errorf("%s offered without its source", absent)
		}
	}
}

func TestTool_SchemasAreStrictObjects(t *testing.T) {
	f := newFixture(t)
	d := f.deps(nil)
	d.Investigations, d.Trust = &fakeInvestigations{}, fakeTrust(`{}`)
	d.Proposer, d.Starter = &fakeProposer{}, &fakeStarter{}
	s := f.service(d)
	for _, tool := range s.newSession(operator).tools() {
		var schema struct {
			Type       string `json:"type"`
			Additional *bool  `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil ||
			schema.Type != "object" || schema.Additional == nil || *schema.Additional {
			t.Errorf("%s schema %s is not a strict object", tool.Name, tool.Parameters)
		}
		if tool.Cost != 1 {
			t.Errorf("%s costs %d, want 1", tool.Name, tool.Cost)
		}
	}
}
