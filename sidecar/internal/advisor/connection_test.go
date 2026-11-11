package advisor

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// advisors-10: the connection prompt is checked by the facts it computes
// from the snapshot and the review environment (advisors-06 platform and
// documentation ranges), not by phrases of its static rules text.

// No concurrent access tests: each case builds its own advisor and
// capturing LLM; TestPromptPlatform_ParallelAdvisorsNameTheirOwn covers
// parallel reviews.

var (
	dataFactLine  = regexp.MustCompile(`^\s*([A-Za-z_ ]+?)(?:: | = )(.+)$`)
	platformLine  = regexp.MustCompile(`(?m)^Platform: (\S+) \(([^)]*)\)`)
	docRangeLine  = regexp.MustCompile(`(?m)^- ([a-z_]+): .*? Safe range: (\S+?)\. `)
	docBaseUnit   = regexp.MustCompile(`(?m)^- ([a-z_]+): .*?A value without a unit is in (\S+?)\.`)
	docNoteMarker = regexp.MustCompile(`(?m)^- ([a-z_]+): .*?Version note \(PG(\d+)\)`)
)

// promptFacts reads the "name: value" and "name = value" lines of the
// untrusted data block; headings and fence lines carry no value.
func promptFacts(user string) map[string]string {
	facts := map[string]string{}
	for _, line := range strings.Split(user, "\n") {
		if m := dataFactLine.FindStringSubmatch(line); m != nil {
			facts[strings.TrimSpace(m[1])] = strings.TrimSpace(m[2])
		}
	}
	return facts
}

// systemFacts are the computed facts of a system prompt's trusted part.
type systemFacts struct {
	platform, kind string
	ranges, units  map[string]string
	notes          map[string]string // guc -> PG major of its version note
}

func readSystemFacts(system string) systemFacts {
	f := systemFacts{ranges: map[string]string{}, units: map[string]string{},
		notes: map[string]string{}}
	if m := platformLine.FindStringSubmatch(system); m != nil {
		f.platform, f.kind = m[1], m[2]
	}
	for _, m := range docRangeLine.FindAllStringSubmatch(system, -1) {
		f.ranges[m[1]] = m[2]
	}
	for _, m := range docBaseUnit.FindAllStringSubmatch(system, -1) {
		f.units[m[1]] = m[2]
	}
	for _, m := range docNoteMarker.FindAllStringSubmatch(system, -1) {
		f.notes[m[1]] = m[2]
	}
	return f
}

func connectionSnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		ConfigData: &collector.ConfigSnapshot{
			PGSettings: []collector.PGSetting{
				{Name: "max_connections", Setting: "200", Context: "postmaster"},
				{Name: "superuser_reserved_connections", Setting: "3"},
				{Name: "idle_in_transaction_session_timeout", Setting: "0", Unit: "ms"},
				{Name: "work_mem", Setting: "4096", Unit: "kB"},
			},
			ConnectionStates: []collector.ConnectionState{
				{State: "active", Count: 12, AvgDurationSeconds: 3.4},
				{State: "idle in transaction", Count: 2, AvgDurationSeconds: 95.6},
			},
		},
		System: collector.SystemStats{ActiveBackends: 12, TotalBackends: 41,
			MaxConnections: 200},
	}
}

// connectionPrompt runs analyzeConnections with a review environment and
// returns the one prompt it sent.
func connectionPrompt(t *testing.T, env promptEnv) capturedPrompt {
	t.Helper()
	mgr, capture := capturingManager(t, "[]")
	ctx := context.WithValue(context.Background(), promptEnvKey{}, env)
	if _, err := analyzeConnections(ctx, mgr, connectionSnapshot(), config.DefaultConfig(),
		noopLog); err != nil {
		t.Fatalf("analyzeConnections: %v", err)
	}
	return onlyPrompt(t, capture())
}

func TestConnectionPrompt_ComputedFacts(t *testing.T) {
	p := connectionPrompt(t, promptEnv{platform: "rds", pgMajor: 17})
	want := map[string]string{
		"max_connections":                     "200",
		"superuser_reserved_connections":      "3",
		"idle_in_transaction_session_timeout": "0ms",
		"active":                              "count=12, avg_duration=3s",
		"idle in transaction":                 "count=2, avg_duration=96s",
		"Advisor window":                      "24h0m0s",
		"Active backends":                     "12",
		"Sessions opened in the window":       churnUnknown,
		"Total backends":                      "41",
		"Peak total backends in the window":   "unknown",
		"Max connections":                     "200",
		"Reserved slots":                      "3",
		"Lowest safe max_connections":         floorUnknown,
	}
	if got := promptFacts(p.user); !reflect.DeepEqual(got, want) {
		t.Errorf("connection facts:\n got %v\nwant %v", got, want)
	}
}

func TestConnectionPrompt_PlatformAndDocRangeFacts(t *testing.T) {
	cases := []struct {
		env      promptEnv
		platform string
		kind     string
	}{
		{promptEnv{platform: "rds", pgMajor: 17}, "rds", "managed service"},
		{promptEnv{platform: "neon", pgMajor: 16}, "neon", "managed service"},
		{promptEnv{pgMajor: 14}, "self-managed", "ALTER SYSTEM is available; " +
			"restart-only settings need an operator-scheduled restart"},
	}
	for _, c := range cases {
		f := readSystemFacts(connectionPrompt(t, c.env).system)
		if f.platform != c.platform || f.kind != c.kind {
			t.Errorf("%+v: platform %q (%q), want %q (%q)", c.env, f.platform, f.kind,
				c.platform, c.kind)
		}
		want := map[string]string{"max_connections": "10..10000",
			"idle_in_transaction_session_timeout": "60000ms..86400000ms"}
		if !reflect.DeepEqual(f.ranges, want) {
			t.Errorf("%+v: doc ranges %v, want %v", c.env, f.ranges, want)
		}
		units := map[string]string{"idle_in_transaction_session_timeout": "ms"}
		if !reflect.DeepEqual(f.units, units) || len(f.notes) != 0 {
			t.Errorf("%+v: base units %v (want %v), version notes %v (want none)",
				c.env, f.units, units, f.notes)
		}
	}
}

func TestConnectionPrompt_NoConfigDataSendsNothing(t *testing.T) {
	mgr, capture := capturingManager(t, "[]")
	got, err := analyzeConnections(context.Background(), mgr, &collector.Snapshot{},
		config.DefaultConfig(), noopLog)
	if err != nil || got != nil || len(capture()) != 0 {
		t.Errorf("nil ConfigData: findings %v, err %v, prompts %d", got, err,
			len(capture()))
	}
}
