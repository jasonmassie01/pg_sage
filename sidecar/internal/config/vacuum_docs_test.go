package config

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// vacuum-11: the vacuum, bloat and freeze docs must describe the code. The
// configuration section names every freeze custodian key with its default,
// states the cadence the runtime derives, and the architecture overview
// and the findings catalog stop making claims the code does not keep.
//
// No nil/empty, error propagation, concurrency, state transition or
// integration tests: these are static checks over checked-in docs.

const (
	configurationDocPath = "../../../docs/configuration.md"
	architectureDocPath  = "../../../docs/architecture.md"
	findingsDocPath      = "../../../docs/findings.md"
	analyzerSourceDir    = "../analyzer"
	vacuumSectionHeading = "### Vacuum, bloat and freeze autopilot"
)

// goSourceToken is a Go file name in backticks, as a catalog source names it.
var goSourceToken = regexp.MustCompile("`([a-z0-9_/]+\\.go)`")

func readVacuumDoc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// normalizeSpace folds every run of whitespace to one space, so a phrase
// wrapped across lines still matches.
func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// docSection returns the text under heading up to the next heading of the
// same or a higher level ("" when the heading is missing). A "# " line in a
// code fence (a shell or YAML comment) is not a heading.
func docSection(doc, heading string) string {
	level := strings.Index(heading, " ")
	start := -1
	inFence := false
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		if start < 0 {
			if strings.TrimSpace(line) == heading {
				start = i + 1
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		hashes := len(line) - len(strings.TrimLeft(line, "#"))
		if hashes > 0 && hashes <= level && strings.HasPrefix(line[hashes:], " ") {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start < 0 {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

func TestVacuumDocs_SectionSpansCodeFenceComments(t *testing.T) {
	doc := "### a\nfirst\n```sh\n# a shell comment\n```\nSAFE-tier\n### b\nsecond\n"
	got := docSection(doc, "### a")
	if !strings.Contains(got, "SAFE-tier") || strings.Contains(got, "second") {
		t.Fatalf("section = %q, want it to run past the fenced comment and stop at ### b", got)
	}
	if got := docSection(doc, "### missing"); got != "" {
		t.Fatalf("missing heading returned %q, want empty", got)
	}
}

func vacuumSection(t *testing.T) string {
	t.Helper()
	section := docSection(readVacuumDoc(t, configurationDocPath), vacuumSectionHeading)
	if strings.TrimSpace(section) == "" {
		t.Fatalf("docs/configuration.md has no %q section", vacuumSectionHeading)
	}
	return section
}

// freezeCustodianDefaults maps each custodian.freeze.* key of Config to its
// default from DefaultConfig, formatted as the docs write it.
func freezeCustodianDefaults(t *testing.T) map[string]string {
	t.Helper()
	value := reflect.ValueOf(DefaultConfig().Custodian.Freeze)
	out := map[string]string{}
	for i := 0; i < value.NumField(); i++ {
		name := strings.Split(value.Type().Field(i).Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("FreezeCustodianConfig.%s has no yaml key", value.Type().Field(i).Name)
		}
		out["custodian.freeze."+name] = formatDefault(value.Field(i))
	}
	if len(out) == 0 {
		t.Fatal("FreezeCustodianConfig has no keys; the reflection walk is broken")
	}
	return out
}

func formatDefault(v reflect.Value) string {
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	case reflect.Int, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Bool:
		return strconv.FormatBool(v.Bool())
	}
	return v.String()
}

// undocumentedKeys returns one problem per key that the section does not
// name in backticks on a line that also shows its default in backticks.
func undocumentedKeys(section string, defaults map[string]string) []string {
	var problems []string
	for key, def := range defaults {
		named, withDefault := false, false
		for _, line := range strings.Split(section, "\n") {
			if !strings.Contains(line, "`"+key+"`") {
				continue
			}
			named = true
			if strings.Contains(line, "`"+def+"`") {
				withDefault = true
			}
		}
		switch {
		case !named:
			problems = append(problems, key+" is not named in the section")
		case !withDefault:
			problems = append(problems, key+" is named without its default `"+def+"`")
		}
	}
	return problems
}

func TestVacuumDocs_FreezeKeysDocumentedWithDefaults(t *testing.T) {
	defaults := freezeCustodianDefaults(t)
	if got := defaults["custodian.freeze.red_buffer_pct"]; got != "25" {
		t.Fatalf("red_buffer_pct default formats as %q, want 25 (defaults.go)", got)
	}
	for _, problem := range undocumentedKeys(vacuumSection(t), defaults) {
		t.Errorf("docs/configuration.md %s: %s", vacuumSectionHeading, problem)
	}
}

func TestVacuumDocs_FixtureMissingKeyIsNamed(t *testing.T) {
	defaults := map[string]string{"custodian.freeze.red_buffer_pct": "25"}
	fixture := "| `custodian.freeze.amber` | `50` | something else |\n"
	problems := undocumentedKeys(fixture, defaults)
	if len(problems) != 1 || !strings.Contains(problems[0], "custodian.freeze.red_buffer_pct") ||
		!strings.Contains(problems[0], "not named") {
		t.Fatalf("problems = %q, want one naming custodian.freeze.red_buffer_pct", problems)
	}
}

func TestVacuumDocs_FixtureWrongDefaultIsNamed(t *testing.T) {
	defaults := map[string]string{"custodian.freeze.red_buffer_pct": "25"}
	fixture := "| `custodian.freeze.red_buffer_pct` | `250` | remaining runway |\n"
	problems := undocumentedKeys(fixture, defaults)
	if len(problems) != 1 || !strings.Contains(problems[0], "without its default `25`") {
		t.Fatalf("problems = %q, want the missing default `25` named", problems)
	}
	ok := "| `custodian.freeze.red_buffer_pct` | `25` | remaining runway |\n"
	if got := undocumentedKeys(ok, defaults); len(got) != 0 {
		t.Fatalf("a row with key and default reported %q", got)
	}
}

func TestVacuumDocs_CadenceDerivesFromAnalyzerInterval(t *testing.T) {
	section := normalizeSpace(vacuumSection(t))
	if !strings.Contains(section, "`analyzer.interval_seconds`") {
		t.Error("the section must state the custodian cadence from analyzer.interval_seconds")
	}
	if strings.Contains(strings.ToLower(section), "every minute") {
		t.Error("the section says \"every minute\"; the cadence is max(analyzer " +
			"interval, 1 minute), 10 minutes by default")
	}
	def := strconv.Itoa(DefaultConfig().Analyzer.IntervalSeconds)
	if !strings.Contains(section, "`"+def+"`") {
		t.Errorf("the section must give the default interval `%s` seconds", def)
	}
}

func TestVacuumDocs_SectionCoversLadderApprovalAndLedger(t *testing.T) {
	section := normalizeSpace(vacuumSection(t))
	for _, want := range []string{
		"`VACUUM (FREEZE)`", "`freeze_blocker`", "`plan_catalog_freeze`",
		"`diagnose_freeze_blockers`", "`custodian_backoff`", "`custodian_escalation`",
		"policy-gate.md", "`sage.decision`", "`analyzer.wraparound_freeze_share`",
		"`analyzer.wraparound_freeze_xid_age`", "one freeze custodian",
		"always waits for an operator's approval",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("the section does not mention %s", want)
		}
	}
}

// retiredVacuumKeys are keys the section names only to say they are retired.
var retiredVacuumKeys = map[string]bool{"analyzer.wraparound_freeze_xid_age": true}

func TestVacuumDocs_SectionConfigKeysExist(t *testing.T) {
	checked := 0
	for _, m := range backtickToken.FindAllStringSubmatch(vacuumSection(t), -1) {
		token := strings.TrimSuffix(m[1], ":")
		if !dottedKey.MatchString(token) || notConfigKey.MatchString(token) {
			continue
		}
		parts := strings.Split(token, ".")
		// Only a config section's keys: detail.* and evidence.* are fields.
		if !yamlPathExists(reflect.TypeOf(Config{}), parts[:1]) {
			continue
		}
		checked++
		if retiredVacuumKeys[token] {
			if _, ok := retiredKeyMessage(parts); !ok {
				t.Errorf("%s is documented as retired but is not in retiredKeys", token)
			}
			continue
		}
		if !yamlPathExists(reflect.TypeOf(Config{}), parts) {
			t.Errorf("the section names config key %q, which does not exist", token)
		}
	}
	if checked < 5 {
		t.Fatalf("only %d config keys found in the section", checked)
	}
}

func retiredKeyMessage(parts []string) (string, bool) {
	for _, group := range retiredKeys {
		if strings.Join(group.path, ".") != strings.Join(parts[:len(parts)-1], ".") {
			continue
		}
		msg, ok := group.keys[parts[len(parts)-1]]
		return msg, ok
	}
	return "", false
}

func TestVacuumDocs_ArchitectureDropsBackendXminClaim(t *testing.T) {
	doc := normalizeSpace(readVacuumDoc(t, architectureDocPath))
	if strings.Contains(doc, "diagnose oldest `backend_xmin` holders") {
		t.Error("docs/architecture.md still says wraparound cases diagnose oldest " +
			"backend_xmin holders; the freeze custodian runs a response ladder")
	}
	for _, want := range []string{
		"`VACUUM (FREEZE)`", "configuration.md#vacuum-bloat-and-freeze-autopilot",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/architecture.md autopilot paragraph does not mention %s", want)
		}
	}
}

func TestVacuumDocs_BudgetBypassUsesWarnLimitDeadline(t *testing.T) {
	doc := normalizeSpace(readVacuumDoc(t, configurationDocPath))
	i := strings.Index(doc, "**Emergency mitigations bypass the kind budgets.**")
	if i < 0 {
		t.Fatal("docs/configuration.md lost the budget bypass paragraph")
	}
	end := min(len(doc), i+700)
	if !strings.Contains(doc[i:end], "wraparound warn limit") {
		t.Error("the budget bypass must define the XID hard deadline as the " +
			"wraparound warn limit (vacuum-03)")
	}
	if strings.Contains(doc, "as the custodian computed it") {
		t.Error("docs/configuration.md keeps the pre-vacuum-03 deadline wording")
	}
}

// maintenanceFindings are the vacuum rules' catalog sections.
var maintenanceFindings = []string{
	"table_bloat", "autovacuum_tuning", "xid_wraparound", "wraparound_freeze",
}

// missingSources returns the Go files doc names that are not in dir.
func missingSources(doc, dir string) []string {
	var missing []string
	for _, m := range goSourceToken.FindAllStringSubmatch(doc, -1) {
		if _, err := os.Stat(filepath.Join(dir, filepath.Base(m[1]))); err != nil {
			missing = append(missing, m[1])
		}
	}
	return missing
}

func TestVacuumDocs_FindingsSourcesExist(t *testing.T) {
	doc := readVacuumDoc(t, findingsDocPath)
	for _, name := range missingSources(doc, analyzerSourceDir) {
		t.Errorf("docs/findings.md names %s, which is not under internal/analyzer", name)
	}
	for _, category := range maintenanceFindings {
		section := docSection(doc, "### "+category)
		if strings.TrimSpace(section) == "" {
			t.Errorf("docs/findings.md has no ### %s section", category)
			continue
		}
		if !goSourceToken.MatchString(section) {
			t.Errorf("docs/findings.md ### %s names no source file", category)
		}
	}
}

func TestVacuumDocs_FixtureMissingSourceIsNamed(t *testing.T) {
	fixture := "**Source:** `rules_wraparound.go` and `rules_vacuum.go`"
	missing := missingSources(fixture, analyzerSourceDir)
	if len(missing) != 1 || missing[0] != "rules_wraparound.go" {
		t.Fatalf("missing = %q, want only rules_wraparound.go", missing)
	}
}

func TestVacuumDocs_FindingsThresholdAndTierMatchCode(t *testing.T) {
	doc := readVacuumDoc(t, findingsDocPath)
	bloat := normalizeSpace(docSection(doc, "### table_bloat"))
	pct := "`" + strconv.Itoa(DefaultTableBloatDeadTuplePct) + "`"
	if !strings.Contains(bloat, "`analyzer.table_bloat_dead_tuple_pct`") ||
		!strings.Contains(bloat, pct) {
		t.Errorf("### table_bloat must name analyzer.table_bloat_dead_tuple_pct and "+
			"its default %s", pct)
	}
	if strings.Contains(doc, "Dead >10%") {
		t.Error("docs/findings.md keeps the wrong 10% table_bloat threshold")
	}
	tuning := normalizeSpace(docSection(doc, "### autovacuum_tuning"))
	// set_table_autovacuum's contract is moderate (executor/contract_maintenance.go).
	if strings.Contains(strings.ToLower(tuning), "safe-tier") ||
		!strings.Contains(tuning, "`moderate`") {
		t.Error("### autovacuum_tuning must give its contract tier `moderate`, not SAFE")
	}
}
