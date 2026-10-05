package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Self-configuration (roadmap phase 3): every configuration key is
// classified as safety-critical (never derived), operator-preference
// (never derived) or derivable. The classification is data
// (key_classes.txt); a key added to Config fails these tests until it is
// classified, and a classified key that no longer exists fails them too.

func allConfigPaths() []string {
	var paths []string
	for _, f := range FieldLifecycles() {
		paths = append(paths, f.Path)
	}
	for _, f := range DatabaseFieldLifecycles() {
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)
	return paths
}

func TestEveryConfigKeyIsClassified(t *testing.T) {
	paths := allConfigPaths()
	if len(paths) < 300 {
		t.Fatalf("registry walked only %d keys; expected the full config surface", len(paths))
	}
	var missing []string
	for _, path := range paths {
		class, ok := KeyClassOf(path)
		if !ok {
			missing = append(missing, path)
			continue
		}
		switch class {
		case KeySafetyCritical, KeyOperatorPreference, KeyDerivable:
		default:
			t.Errorf("%s: unknown class %q", path, class)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("%d config keys are not classified in key_classes.txt "+
			"(safety_critical, operator_preference or derivable): %v",
			len(missing), missing)
	}
}

func TestClassificationHasNoStaleKeys(t *testing.T) {
	known := map[string]bool{}
	for _, path := range allConfigPaths() {
		known[path] = true
	}
	for path := range KeyClassification() {
		if !known[path] {
			t.Errorf("key_classes.txt classifies %q, which is not a config key", path)
		}
	}
}

func TestKeyClassificationIsACopy(t *testing.T) {
	first := KeyClassification()
	first["collector.interval_seconds"] = KeySafetyCritical
	first["invented.key"] = KeyDerivable
	if class, _ := KeyClassOf("collector.interval_seconds"); class != KeyDerivable {
		t.Fatalf("mutating the returned map changed the registry: %q", class)
	}
	if _, ok := KeyClassOf("invented.key"); ok {
		t.Fatal("mutating the returned map added a key to the registry")
	}
}

func TestKeyClassOfUnknownAndEmpty(t *testing.T) {
	for _, path := range []string{"", "collector", "collector.", "nope.nope",
		"COLLECTOR.INTERVAL_SECONDS", " collector.interval_seconds"} {
		if class, ok := KeyClassOf(path); ok || class != "" {
			t.Errorf("KeyClassOf(%q) = %q, %v; want unclassified", path, class, ok)
		}
	}
}

func TestParseKeyClassesRejectsBadData(t *testing.T) {
	cases := map[string]string{
		"unknown class":   "derivable a.b\nmaybe c.d\n",
		"duplicate key":   "derivable a.b\nsafety_critical a.b\n",
		"missing key":     "derivable\n",
		"extra field":     "derivable a.b c\n",
		"key with spaces": "derivable  \n",
	}
	for name, data := range cases {
		if _, err := parseKeyClasses(data); err == nil {
			t.Errorf("%s: parseKeyClasses accepted %q", name, data)
		}
	}
	got, err := parseKeyClasses("# comment\n\nderivable a.b\n" +
		"operator_preference c.d # trailing\nsafety_critical e.f\n")
	if err != nil {
		t.Fatalf("valid data rejected: %v", err)
	}
	want := map[string]KeyClass{"a.b": KeyDerivable, "c.d": KeyOperatorPreference,
		"e.f": KeySafetyCritical}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if got, err := parseKeyClasses(""); err != nil || len(got) != 0 {
		t.Fatalf("empty data: %v, %v", got, err)
	}
}

// Keys that grant authority, hold credentials or name endpoints are never
// derivable, whatever their section.
func TestAuthorityCredentialAndEndpointKeysAreSafetyCritical(t *testing.T) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`^trust\.(level|tier3_|ramp_|rollback_|cascade_)`),
		regexp.MustCompile(`(^|\.)(password|api_key|client_secret|encryption_key|` +
			`hmac_secret|bearer_token|bearer_token_file|dle_token|routing_key)$`),
		regexp.MustCompile(`(^|\.)(endpoint|url|database_url|issuer_url|redirect_url|` +
			`dle_endpoint|local_dsn|host|listen_addr)$`),
		regexp.MustCompile(`^(postgres|oauth|mcp|agentdb|azure|api)\.`),
		regexp.MustCompile(`^sre\.(autonomy|actions)\.`),
		regexp.MustCompile(`^(mode|meta_db|encryption_key|policy\.profile)$`),
		regexp.MustCompile(`^llm\.(enabled|model|endpoint|api_key|json_mode)$`),
		regexp.MustCompile(`^llm\.optimizer_llm\.(enabled|model|endpoint|api_key)$`),
		regexp.MustCompile(`^self_config\.`),
		regexp.MustCompile(`^(defaults\.|databases\[\]\.)(trust_level|execution_mode|` +
			`executor_enabled|llm_enabled)$`),
		regexp.MustCompile(`^runaway\.`),
		regexp.MustCompile(`^custodian\.`),
		regexp.MustCompile(`^verify\.(drop_window_hours|io_baseline_|min_gain_pct|` +
			`regress_pct|window_|write_impact_pct|min_samples)`),
	}
	for _, path := range allConfigPaths() {
		for _, re := range patterns {
			if !re.MatchString(path) {
				continue
			}
			if class, _ := KeyClassOf(path); class != KeySafetyCritical {
				t.Errorf("%s matches %s but is classified %q, want safety_critical",
					path, re, class)
			}
		}
	}
}

// Notification routes and windows the operator declares are preferences:
// never derived, never safety gates.
func TestOperatorDeclaredRoutesAndWindowsArePreferences(t *testing.T) {
	for _, path := range []string{
		"alerting.routes", "alerting.quiet_hours_start", "alerting.quiet_hours_end",
		"alerting.timezone", "briefing.schedule", "briefing.channels",
		"trust.maintenance_window", "forecaster.disk_capacity_bytes",
		"verify.io_capacity", "llm.fleet_token_budget_daily",
		"defaults.collector_interval_seconds", "databases[].collector_interval_seconds",
		"retention.actions_days",
	} {
		if class, ok := KeyClassOf(path); !ok || class != KeyOperatorPreference {
			t.Errorf("%s = %q (%v), want operator_preference", path, class, ok)
		}
	}
}

// The keys self-configuration starts with must be classified derivable.
func TestInitialDerivedKeysAreDerivable(t *testing.T) {
	for _, path := range []string{
		"collector.interval_seconds", "safety.query_timeout_ms",
		"sre.runways.sequence_interval_seconds", "sre.detectors.temp_file_mb",
		"sre.detectors.lwlock_waiters",
	} {
		if class, ok := KeyClassOf(path); !ok || class != KeyDerivable {
			t.Errorf("%s = %q (%v), want derivable", path, class, ok)
		}
	}
}

// No environment overlay may set a derivable key: an environment value is
// an operator setting the derivation layer cannot see in the YAML file.
func TestEnvironmentOverlaidKeysAreNeverDerivable(t *testing.T) {
	names := sageEnvNames(t)
	if len(names) < 20 {
		t.Fatalf("found only %d SAGE_* environment names in the config sources", len(names))
	}
	changed := map[string]bool{}
	for _, value := range []string{"7", "true", "x"} {
		for _, name := range names {
			t.Setenv(name, value)
		}
		overlaid := DefaultConfig()
		overlayEnv(overlaid)
		for _, path := range changedConfigPaths(DefaultConfig(), overlaid) {
			changed[path] = true
		}
	}
	if len(changed) == 0 {
		t.Fatal("setting every SAGE_* variable changed no config key")
	}
	for path := range changed {
		if class, _ := KeyClassOf(path); class == KeyDerivable {
			t.Errorf("environment overlays set %s, which is classified derivable", path)
		}
	}
}

func sageEnvNames(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`"(SAGE_[A-Z0-9_]+)"`)
	seen := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(body), -1) {
			seen[m[1]] = true
		}
	}
	delete(seen, "SAGE_CONFIG_PATH")
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestLifecycleReferenceShowsTheSelfConfigClass(t *testing.T) {
	doc := ConfigLifecycleMarkdown()
	if !strings.Contains(doc, "| Field | Lifecycle | Runtime owner | Self-config class |") {
		t.Fatal("lifecycle reference has no self-config class column")
	}
	if !strings.Contains(doc,
		"| `collector.interval_seconds` | `reconfigure` | `collector` | `derivable` |") {
		t.Fatal("collector.interval_seconds row does not carry its class")
	}
	if !strings.Contains(doc, "| `trust.level` | `live_policy` | `trust_policy` | "+
		"`safety_critical` |") {
		t.Fatal("trust.level row does not carry its class")
	}
}
