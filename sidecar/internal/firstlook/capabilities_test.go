package firstlook

import (
	"strings"
	"testing"
)

// No concurrent access tests here: ExtensionCapabilities is a pure function.

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func capability(caps []Capability, name string) Capability {
	for _, c := range caps {
		if c.Name == name {
			return c
		}
	}
	return Capability{}
}

func allReady() Extensions {
	return Extensions{StatStatementsInstalled: true, StatStatementsLoaded: boolp(true),
		QueryTextVisible: boolp(true), HypoPGInstalled: true, HypoPGAvailable: true,
		AutoExplainLoaded: true, PreloadLibraries: strp("pg_stat_statements,auto_explain")}
}

func TestExtensionCapabilitiesAllReady(t *testing.T) {
	caps, items := ExtensionCapabilities(allReady(), "self-managed")
	if len(items) != 0 {
		t.Fatalf("all ready gave items %v", rulesOf(items))
	}
	for _, name := range []string{CapStatStatements, CapHypoPG, CapAutoExplain} {
		if c := capability(caps, name); c.Status != CapabilityOK {
			t.Fatalf("%s = %+v, want ok", name, c)
		}
	}
}

func TestStatStatementsNotLoadedSelfManaged(t *testing.T) {
	e := allReady()
	e.StatStatementsInstalled, e.StatStatementsLoaded = false, boolp(false)
	e.PreloadLibraries = strp("auto_explain")
	caps, items := ExtensionCapabilities(e, "self-managed")
	c := capability(caps, CapStatStatements)
	if c.Status != CapabilityNotLoaded {
		t.Fatalf("pg_stat_statements = %+v, want not_loaded", c)
	}
	steps := strings.Join(c.Steps, "\n")
	for _, want := range []string{
		"ALTER SYSTEM SET shared_preload_libraries = 'auto_explain', 'pg_stat_statements';",
		"restart", "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"} {
		if !strings.Contains(steps, want) {
			t.Fatalf("steps %q lack %q", steps, want)
		}
	}
	if len(items) == 0 || items[0].Rule != RuleMissingExtension ||
		items[0].Object != CapStatStatements || items[0].Severity != SeverityWarning {
		t.Fatalf("items = %+v, want a pg_stat_statements warning", items)
	}
	requireEvidence(t, items[0])
}

func TestStatStatementsLoadedButNotCreated(t *testing.T) {
	e := allReady()
	e.StatStatementsInstalled = false
	caps, _ := ExtensionCapabilities(e, "self-managed")
	c := capability(caps, CapStatStatements)
	if c.Status != CapabilityMissing || len(c.Steps) != 1 ||
		!strings.Contains(c.Steps[0], "CREATE EXTENSION IF NOT EXISTS pg_stat_statements") {
		t.Fatalf("pg_stat_statements = %+v, want one CREATE EXTENSION step", c)
	}
}

func TestStatStatementsUnknownPreloadIsStated(t *testing.T) {
	e := allReady()
	e.StatStatementsInstalled, e.StatStatementsLoaded, e.PreloadLibraries = false, nil, nil
	caps, _ := ExtensionCapabilities(e, "self-managed")
	steps := strings.Join(capability(caps, CapStatStatements).Steps, "\n")
	if !strings.Contains(steps, "<existing libraries>") {
		t.Fatalf("steps %q should keep a placeholder for unreadable preload libraries", steps)
	}
}

func TestManagedProviderSteps(t *testing.T) {
	e := allReady()
	e.StatStatementsInstalled, e.StatStatementsLoaded = false, boolp(false)
	e.HypoPGInstalled, e.AutoExplainLoaded = false, false
	cases := map[string][]string{
		"lakebase":  {"Lakebase manages", "CREATE EXTENSION"},
		"rds":       {"parameter group", "shared_preload_libraries", "reboot"},
		"aurora":    {"parameter group", "shared_preload_libraries"},
		"cloud-sql": {"Cloud SQL", "database flag"},
		"alloydb":   {"AlloyDB", "database flag"},
		"azure":     {"azure.extensions", "shared_preload_libraries"},
	}
	for provider, wants := range cases {
		t.Run(provider, func(t *testing.T) {
			caps, _ := ExtensionCapabilities(e, provider)
			steps := strings.Join(capability(caps, CapStatStatements).Steps, "\n")
			for _, want := range wants {
				if !strings.Contains(steps, want) {
					t.Fatalf("%s steps %q lack %q", provider, steps, want)
				}
			}
			if strings.Contains(steps, "ALTER SYSTEM") {
				t.Fatalf("%s steps %q must not tell a managed user to ALTER SYSTEM",
					provider, steps)
			}
		})
	}
}

func TestHypoPGAndAutoExplain(t *testing.T) {
	e := allReady()
	e.HypoPGInstalled, e.HypoPGAvailable = false, true
	e.AutoExplainLoaded = false
	caps, items := ExtensionCapabilities(e, "self-managed")
	h := capability(caps, CapHypoPG)
	if h.Status != CapabilityMissing || !strings.Contains(strings.Join(h.Steps, " "),
		"CREATE EXTENSION IF NOT EXISTS hypopg") {
		t.Fatalf("hypopg = %+v", h)
	}
	a := capability(caps, CapAutoExplain)
	if a.Status != CapabilityNotLoaded || !strings.Contains(strings.Join(a.Steps, " "),
		"auto_explain") {
		t.Fatalf("auto_explain = %+v", a)
	}
	for _, it := range items {
		if it.Severity != SeverityInfo {
			t.Fatalf("optional extension item %+v should be info", it)
		}
	}
	e.HypoPGAvailable = false
	caps, _ = ExtensionCapabilities(e, "self-managed")
	if h = capability(caps, CapHypoPG); h.Status != CapabilityUnavailable ||
		!strings.Contains(strings.Join(h.Steps, " "), "package") {
		t.Fatalf("hypopg not available = %+v, want install-the-package step", h)
	}
}

func TestQueryTextHidden(t *testing.T) {
	e := allReady()
	e.QueryTextVisible = boolp(false)
	_, items := ExtensionCapabilities(e, "self-managed")
	var found *Item
	for i := range items {
		if items[i].Rule == RuleQueryTextHidden {
			found = &items[i]
		}
	}
	if found == nil || !strings.Contains(found.SuggestedSQL, "GRANT pg_read_all_stats") {
		t.Fatalf("items = %+v, want a pg_read_all_stats grant", items)
	}
	e.QueryTextVisible = nil
	if _, items = ExtensionCapabilities(e, "self-managed"); len(items) != 0 {
		t.Fatalf("unknown visibility gave items %v", rulesOf(items))
	}
}

func TestUnknownProviderFallsBackToSelfManaged(t *testing.T) {
	e := allReady()
	e.StatStatementsInstalled, e.StatStatementsLoaded = false, boolp(false)
	for _, p := range []string{"", "unknown", "neon"} {
		caps, _ := ExtensionCapabilities(e, p)
		if steps := capability(caps, CapStatStatements).Steps; len(steps) == 0 {
			t.Fatalf("provider %q gave no steps", p)
		}
	}
}

// The self-managed auto_explain step adds auto_explain to the current
// session_preload_libraries instead of replacing it, which would silently
// unload the operator's other session libraries.
func TestAutoExplainKeepsSessionPreloadLibraries(t *testing.T) {
	cases := []struct {
		name string
		list *string
		want string
	}{
		{"other library kept", strp("pg_hint_plan"), "'pg_hint_plan', 'auto_explain'"},
		{"unreadable setting", nil, "'<existing libraries>', 'auto_explain'"},
		{"already listed, unchanged", strp("auto_explain,pg_hint_plan"),
			"'auto_explain', 'pg_hint_plan'"},
		{"empty list", strp(""), "'auto_explain'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := allReady()
			e.AutoExplainLoaded = false
			e.SessionPreloadLibraries = c.list
			caps, _ := ExtensionCapabilities(e, "self-managed")
			a := capability(caps, CapAutoExplain)
			if a.Status != CapabilityNotLoaded || len(a.Steps) != 3 {
				t.Fatalf("auto_explain = %+v, want three not_loaded steps", a)
			}
			want := "As a superuser: ALTER SYSTEM SET session_preload_libraries = " +
				c.want + ";"
			if a.Steps[0] != want {
				t.Fatalf("first step = %q, want %q", a.Steps[0], want)
			}
			if !strings.Contains(a.Steps[2], "pg_reload_conf") {
				t.Fatalf("reload step = %q", a.Steps[2])
			}
		})
	}
}

// Managed providers keep their own auto_explain steps; the session list
// only shapes the self-managed ALTER SYSTEM step.
func TestAutoExplainManagedStepsIgnoreSessionList(t *testing.T) {
	e := allReady()
	e.AutoExplainLoaded = false
	e.SessionPreloadLibraries = strp("pg_hint_plan")
	for _, p := range []string{"rds", "cloud-sql", "neon"} {
		caps, _ := ExtensionCapabilities(e, p)
		steps := strings.Join(capability(caps, CapAutoExplain).Steps, "\n")
		if strings.Contains(steps, "session_preload_libraries") || steps == "" {
			t.Fatalf("provider %s steps = %q", p, steps)
		}
	}
}

// collection-02: the collector re-checks pg_stat_statements every cycle, so
// the copy promises no pg_sage restart (PostgreSQL's own restart for
// shared_preload_libraries stays in the steps).
func TestStatStatementsCopyNeedsNoSageRestart(t *testing.T) {
	loaded := allReady()
	loaded.StatStatementsInstalled = false
	notLoaded := allReady()
	notLoaded.StatStatementsInstalled, notLoaded.StatStatementsLoaded = false, boolp(false)
	for name, e := range map[string]Extensions{"loaded": loaded, "not loaded": notLoaded} {
		caps, _ := ExtensionCapabilities(e, "self-managed")
		c := capability(caps, CapStatStatements)
		if !strings.Contains(c.Detail, "no pg_sage restart is needed") {
			t.Errorf("%s: detail %q does not say no pg_sage restart is needed", name, c.Detail)
		}
	}
	ready, _ := ExtensionCapabilities(allReady(), "self-managed")
	if c := capability(ready, CapStatStatements); strings.Contains(c.Detail, "restart") {
		t.Errorf("ready detail %q mentions a restart", c.Detail)
	}
}
