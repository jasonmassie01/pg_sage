package firstlook

import (
	"strings"
	"testing"
)

// No concurrent access tests here: LogRCACapability and LogRCAItem are pure.

func stderrPG17() LogSettings {
	return LogSettings{Known: true, ServerVersionNum: 170002, Destination: "stderr",
		LoggingCollector: "off", LCMessages: "en_US.UTF-8"}
}

// Happy path: jsonlog with the collector on is ready and needs no step.
func TestLogRCAReadyOnJSONLog(t *testing.T) {
	s := LogSettings{Known: true, ServerVersionNum: 170002, Destination: "stderr,jsonlog",
		LoggingCollector: "on", LCMessages: "C"}
	c := LogRCACapability(s, "self-managed")
	if c.Name != CapLogRCA || c.Status != CapabilityOK || len(c.Steps) != 0 {
		t.Fatalf("ready log_rca = %+v", c)
	}
	if _, ok := LogRCAItem(c); ok {
		t.Fatal("a ready log source produced a first-look item")
	}
}

// Error propagation: a stderr server gets the exact settings to change.
func TestLogRCAStderrSelfManagedSteps(t *testing.T) {
	c := LogRCACapability(stderrPG17(), "self-managed")
	if c.Status != CapabilityMissing {
		t.Fatalf("stderr log_rca status = %s, want missing", c.Status)
	}
	steps := strings.Join(c.Steps, "\n")
	for _, want := range []string{"ALTER SYSTEM SET log_destination = 'stderr,jsonlog'",
		"ALTER SYSTEM SET logging_collector = on", "lc_messages", "restart",
		"read-only", "logwatch.enabled", "rca.enabled"} {
		if !strings.Contains(steps, want) {
			t.Fatalf("steps %q lack %q", steps, want)
		}
	}
	it, ok := LogRCAItem(c)
	if !ok || it.Rule != RuleLogRCA || it.Severity != SeverityInfo || it.Object != CapLogRCA ||
		!strings.Contains(it.Recommendation, "log_destination") {
		t.Fatalf("item = %+v, want an info log_rca item with the steps", it)
	}
	requireEvidence(t, it)
}

// Boundary: jsonlog needs PostgreSQL 15; 14 is told to use csvlog.
func TestLogRCAVersionBoundary(t *testing.T) {
	s := stderrPG17()
	s.ServerVersionNum = 140010
	steps := strings.Join(LogRCACapability(s, "self-managed").Steps, "\n")
	if !strings.Contains(steps, "'stderr,csvlog'") || strings.Contains(steps, "jsonlog'") {
		t.Fatalf("PG14 steps %q, want csvlog and no jsonlog", steps)
	}
	s.ServerVersionNum = 150000
	steps = strings.Join(LogRCACapability(s, "self-managed").Steps, "\n")
	if !strings.Contains(steps, "'stderr,jsonlog'") {
		t.Fatalf("PG15 steps %q, want jsonlog", steps)
	}
}

// Error propagation: csvlog without the collector writes no file.
func TestLogRCACollectorOff(t *testing.T) {
	s := stderrPG17()
	s.Destination = "csvlog"
	c := LogRCACapability(s, "self-managed")
	steps := strings.Join(c.Steps, "\n")
	if c.Status != CapabilityMissing || !strings.Contains(steps, "logging_collector = on") ||
		strings.Contains(steps, "log_destination") {
		t.Fatalf("collector-off log_rca = %+v, want only the logging_collector step", c)
	}
	if !strings.Contains(c.Detail, "logging_collector") {
		t.Fatalf("detail %q does not name logging_collector", c.Detail)
	}
}

// Managed providers point to their own sink and never to ALTER SYSTEM.
func TestLogRCAManagedProviders(t *testing.T) {
	cases := map[string][]string{
		"rds":       {"CloudWatch"},
		"aurora":    {"CloudWatch"},
		"cloud-sql": {"Cloud Logging"},
		"alloydb":   {"Cloud Logging"},
		"azure":     {"Azure Monitor"},
		"supabase":  {"SAGE_SUPABASE_OBSERVABILITY_TOKEN"},
		"lakebase":  {"lakebase_observability"},
	}
	for provider, wants := range cases {
		t.Run(provider, func(t *testing.T) {
			c := LogRCACapability(stderrPG17(), provider)
			steps := strings.Join(c.Steps, "\n")
			for _, want := range wants {
				if !strings.Contains(steps, want) {
					t.Fatalf("%s steps %q lack %q", provider, steps, want)
				}
			}
			if strings.Contains(steps, "ALTER SYSTEM") {
				t.Fatalf("%s steps tell a managed user to ALTER SYSTEM: %q", provider, steps)
			}
			if c.Status == CapabilityOK {
				t.Fatalf("%s reported log_rca ready", provider)
			}
		})
	}
}

// Supabase delivers logs through its API: ready even though the server
// logs to stderr, once the operator sets the token.
func TestLogRCASupabaseIsAProviderSink(t *testing.T) {
	c := LogRCACapability(stderrPG17(), "supabase")
	if c.Status != CapabilityMissing {
		t.Fatalf("supabase log_rca = %+v, want missing until the sink is configured", c)
	}
}

// Nil/empty: unreadable settings say so instead of guessing.
func TestLogRCAUnknownSettings(t *testing.T) {
	c := LogRCACapability(LogSettings{}, "self-managed")
	if c.Status != CapabilityMissing || !strings.Contains(c.Detail, "could not read") ||
		len(c.Steps) == 0 {
		t.Fatalf("unknown settings log_rca = %+v", c)
	}
	if _, ok := LogRCAItem(Capability{}); ok {
		t.Fatal("an empty capability produced an item")
	}
}

// Invalid input: a non-English lc_messages is reported on a ready source.
func TestLogRCALocaleNoted(t *testing.T) {
	s := LogSettings{Known: true, ServerVersionNum: 170002, Destination: "jsonlog",
		LoggingCollector: "on", LCMessages: "de_DE.UTF-8"}
	c := LogRCACapability(s, "self-managed")
	if c.Status != CapabilityOK || !strings.Contains(c.Detail, "lc_messages") {
		t.Fatalf("German locale log_rca = %+v, want ok with an lc_messages note", c)
	}
}

// Boundary: with the collector already on, only log_destination changes,
// which a reload applies: no restart and no logging_collector step.
func TestLogRCACollectorOnNeedsOnlyAReload(t *testing.T) {
	s := stderrPG17()
	s.LoggingCollector = "on"
	c := LogRCACapability(s, "self-managed")
	steps := strings.Join(c.Steps, "\n")
	if c.Status != CapabilityMissing || !strings.Contains(steps, "log_destination") ||
		!strings.Contains(steps, "pg_reload_conf()") {
		t.Fatalf("collector-on stderr log_rca = %+v, want the destination and a reload", c)
	}
	if strings.Contains(steps, "restart") || strings.Contains(steps, "logging_collector") {
		t.Fatalf("steps %q ask for a restart the server does not need", steps)
	}
}
