package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// llm-10: operators size LLM spend from docs/llm-costing.md, so its
// defaults table must state what the code ships.
//
// No concurrent-access tests: these are pure functions over a string.
// No database: the guide documents config defaults, not SQL.

const llmCostingPath = "../../../docs/llm-costing.md"

// costingRequiredDefaults are the numbers the guide's spend arithmetic
// rests on. Each must appear in the defaults table with this value.
func costingRequiredDefaults() map[string]string {
	return map[string]string{
		"llm.token_budget_daily":                fmt.Sprint(DefaultLLMTokenBudget),
		"llm.cooldown_seconds":                  fmt.Sprint(DefaultLLMCooldownSeconds),
		"llm.optimizer_llm.token_budget_daily":  fmt.Sprint(DefaultOptLLMTokenBudget),
		"llm.optimizer_llm.cooldown_seconds":    fmt.Sprint(DefaultOptLLMCooldownSeconds),
		"advisor.interval_seconds":              fmt.Sprint(DefaultAdvisorIntervalSeconds),
		"briefing.schedule":                     DefaultBriefingSchedule,
		"tuning.max_cases_per_cycle":            fmt.Sprint(DefaultTuningMaxCasesPerCycle),
		"tuning.max_requests_per_cycle":         fmt.Sprint(DefaultTuningMaxRequestsPerCycle),
		"tuning.max_tokens_per_cycle":           fmt.Sprint(DefaultTuningMaxTokensPerCycle),
		"ask.daily_tokens_per_database":         fmt.Sprint(DefaultAskDailyTokensPerDatabase),
		"ask.max_tokens_per_question":           fmt.Sprint(DefaultAskMaxTokensPerQuestion),
		"explain.max_tokens":                    fmt.Sprint(DefaultExplainMaxTokens),
		"llm.optimizer_llm.fallback_to_general": "true",
	}
}

var costingRowRE = regexp.MustCompile("^\\|\\s*`([a-z0-9_.]+)`\\s*\\|([^|]*)\\|")

// parseCostingDefaults reads the first table under "## Defaults": each row
// whose first cell is a backticked key. A backticked value is taken whole
// ("`0 6 * * *`"); otherwise the first word, commas removed ("86,400 (1
// day)" is 86400).
func parseCostingDefaults(doc string) (map[string]string, error) {
	doc = strings.ReplaceAll(doc, "\r\n", "\n")
	start := strings.Index(doc, "\n## Defaults\n")
	if start < 0 {
		return nil, fmt.Errorf("llm-costing.md: no \"## Defaults\" section")
	}
	rows := map[string]string{}
	inTable := false
	for _, line := range strings.Split(doc[start+1:], "\n")[1:] {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "## ") || (inTable && !strings.HasPrefix(line, "|")) {
			break
		}
		m := costingRowRE.FindStringSubmatch(line)
		if strings.HasPrefix(line, "|") {
			inTable = true
		}
		if m == nil {
			continue
		}
		if _, dup := rows[m[1]]; dup {
			return nil, fmt.Errorf("llm-costing.md: key %s listed twice", m[1])
		}
		rows[m[1]] = costingCellValue(m[2])
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("llm-costing.md: \"## Defaults\" has no key table")
	}
	return rows, nil
}

func costingCellValue(cell string) string {
	cell = strings.TrimSpace(cell)
	if strings.HasPrefix(cell, "`") {
		if end := strings.Index(cell[1:], "`"); end >= 0 {
			return cell[1 : end+1]
		}
	}
	word, _, _ := strings.Cut(cell, " ")
	return strings.ReplaceAll(word, ",", "")
}

// costingDefaultProblems lists every way the table disagrees with code:
// a required key missing or wrong, a key that does not exist, or a value
// that is not the shipped default.
func costingDefaultProblems(doc string) []string {
	rows, err := parseCostingDefaults(doc)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	required := costingRequiredDefaults()
	for key, want := range required {
		got, ok := rows[key]
		switch {
		case !ok:
			problems = append(problems, key+": missing from the defaults table")
		case got != want:
			problems = append(problems, fmt.Sprintf("%s: doc says %s, code default %s",
				key, got, want))
		}
	}
	for key, got := range rows {
		if _, checked := required[key]; checked {
			continue
		}
		field, ok := DescribeField(nil, key)
		if !ok {
			problems = append(problems, key+": no such config key")
			continue
		}
		if !field.HasValue {
			problems = append(problems, key+": default cannot be shown")
			continue
		}
		if want := fmt.Sprint(field.Value); got != want {
			problems = append(problems, fmt.Sprintf("%s: doc says %s, code default %s",
				key, got, want))
		}
	}
	return problems
}

func readCostingDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(llmCostingPath)
	if err != nil {
		t.Fatalf("read %s: %v", llmCostingPath, err)
	}
	return string(raw)
}

func TestLLMCostingDefaultsMatchConfig(t *testing.T) {
	for _, p := range costingDefaultProblems(readCostingDoc(t)) {
		t.Error(p)
	}
}

func TestLLMCostingDefaultsParseKnownRows(t *testing.T) {
	rows, err := parseCostingDefaults(readCostingDoc(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := rows["advisor.interval_seconds"]; got != "86400" {
		t.Errorf("advisor.interval_seconds parsed as %q, want 86400", got)
	}
	if got := rows["briefing.schedule"]; got != "0 6 * * *" {
		t.Errorf("briefing.schedule parsed as %q, want \"0 6 * * *\"", got)
	}
}

const costingFixture = "# Guide\n\n## Defaults\n\nIntro.\n\n" +
	"| Key | Default | Meaning |\n|---|---|---|\n%s\nAfter the table.\n" +
	"| `advisor.interval_seconds` | 1 | outside the table |\n"

func costingFixtureWith(rows map[string]string, extra ...string) string {
	var b strings.Builder
	for key, value := range rows {
		fmt.Fprintf(&b, "| `%s` | %s | x |\n", key, value)
	}
	for _, row := range extra {
		b.WriteString(row + "\n")
	}
	return fmt.Sprintf(costingFixture, strings.TrimSuffix(b.String(), "\n"))
}

func correctCostingRows() map[string]string {
	rows := costingRequiredDefaults()
	rows["briefing.schedule"] = "`" + rows["briefing.schedule"] + "`"
	rows["advisor.interval_seconds"] = "86,400 (one day)"
	return rows
}

func TestCostingDefaultProblemsAcceptsCorrectTable(t *testing.T) {
	if p := costingDefaultProblems(costingFixtureWith(correctCostingRows())); len(p) != 0 {
		t.Fatalf("correct table reported problems: %v", p)
	}
}

func TestCostingDefaultProblemsFlagsDrift(t *testing.T) {
	cases := []struct {
		name, row, wantKey, wantText string
		drop                         string
	}{
		{name: "hourly advisor", drop: "advisor.interval_seconds",
			row: "| `advisor.interval_seconds` | 3,600 | x |", wantKey: "advisor.interval_seconds",
			wantText: "doc says 3600"},
		{name: "unknown key", row: "| `llm.no_such_key` | 1 | x |",
			wantKey: "llm.no_such_key", wantText: "no such config key"},
		{name: "secret not showable", row: "| `llm.api_key` | x | x |",
			wantKey: "llm.api_key", wantText: "cannot be shown"},
		{name: "required key missing", drop: "llm.cooldown_seconds",
			wantKey: "llm.cooldown_seconds", wantText: "missing"},
		{name: "non-required key wrong", row: "| `llm.timeout_seconds` | 1 | x |",
			wantKey: "llm.timeout_seconds", wantText: "doc says 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := correctCostingRows()
			delete(rows, tc.drop)
			var extra []string
			if tc.row != "" {
				extra = append(extra, tc.row)
			}
			problems := costingDefaultProblems(costingFixtureWith(rows, extra...))
			if len(problems) != 1 {
				t.Fatalf("want exactly one problem, got %v", problems)
			}
			if !strings.Contains(problems[0], tc.wantKey) ||
				!strings.Contains(problems[0], tc.wantText) {
				t.Fatalf("problem %q does not name %s / %q", problems[0], tc.wantKey, tc.wantText)
			}
		})
	}
}

func TestParseCostingDefaultsEmptyAndMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"no section": "# Guide\n\n| `llm.cooldown_seconds` | 300 | x |\n",
		"no table":   "# Guide\n\n## Defaults\n\nProse only.\n\n## Next\n",
		"table later": "# G\n\n## Defaults\n\nProse.\n\n## Other\n\n" +
			"| `llm.cooldown_seconds` | 300 |\n",
	}
	for name, doc := range cases {
		if rows, err := parseCostingDefaults(doc); err == nil {
			t.Errorf("%s: want error, got rows %v", name, rows)
		}
	}
	dup := costingFixtureWith(nil, "| `llm.cooldown_seconds` | 300 | x |",
		"| `llm.cooldown_seconds` | 300 | x |")
	if _, err := parseCostingDefaults(dup); err == nil ||
		!strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate key: want \"twice\" error, got %v", err)
	}
}

func TestCostingCellValueBoundaries(t *testing.T) {
	cases := map[string]string{
		" 500,000 ":           "500000",
		"86,400 (1 day)":      "86400",
		"`0 6 * * *` (daily)": "0 6 * * *",
		"`unterminated":       "`unterminated",
		"":                    "",
		"0":                   "0",
	}
	for in, want := range cases {
		if got := costingCellValue(in); got != want {
			t.Errorf("costingCellValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// CHECK-09: the hourly advisor figure is gone, in either spelling.
func TestLLMCostingHasNoHourlyAdvisor(t *testing.T) {
	doc := readCostingDoc(t)
	for _, stale := range []string{"3,600s", "3,600 s", "3600s", "24 calls"} {
		if strings.Contains(doc, stale) {
			t.Errorf("llm-costing.md still says %q; advisor.interval_seconds is %d",
				stale, DefaultAdvisorIntervalSeconds)
		}
	}
}

// The monitoring section names surfaces that exist: the status endpoint,
// and no per-call INFO token log (there is none).
func TestLLMCostingMonitoringSurfaces(t *testing.T) {
	doc := readCostingDoc(t)
	if !strings.Contains(doc, "GET /api/v1/llm/status") {
		t.Error("llm-costing.md does not name GET /api/v1/llm/status")
	}
	if strings.Contains(doc, "logs token count at INFO") {
		t.Error("llm-costing.md claims a per-call INFO token log that does not exist")
	}
}

// metricsWithTokensRE is a line that sends readers to /api/v1/metrics for
// a token field; the Prometheus gauge pg_sage_llm_tokens_used_today is fine.
var bareTokensFieldRE = regexp.MustCompile(`(^|[^a-z_])llm_tokens_used_today`)

func TestDocsNeverPairMetricsEndpointWithTokenField(t *testing.T) {
	var files []string
	err := filepath.WalkDir("../../../docs", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk docs: %v", err)
	}
	if len(files) < 10 {
		t.Fatalf("found only %d docs files; wrong root?", len(files))
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if docLinePairsMetricsAndTokens(line) {
				t.Errorf("%s:%d says /api/v1/metrics carries llm_tokens_used_today; "+
					"it returns fleet status only (use GET /api/v1/llm/status)", path, i+1)
			}
		}
	}
}

func docLinePairsMetricsAndTokens(line string) bool {
	return strings.Contains(line, "/api/v1/metrics") && bareTokensFieldRE.MatchString(line)
}

func TestDocLinePairsMetricsAndTokens(t *testing.T) {
	cases := map[string]bool{
		"- REST API: `GET /api/v1/metrics` includes `llm_tokens_used_today`": true,
		"`/api/v1/metrics` and `pg_sage_llm_tokens_used_today`":              false,
		"- Prometheus metric: `pg_sage_llm_tokens_used_today`":               false,
		"`GET /api/v1/metrics` returns fleet status":                         false,
		"": false,
	}
	for line, want := range cases {
		if got := docLinePairsMetricsAndTokens(line); got != want {
			t.Errorf("docLinePairsMetricsAndTokens(%q) = %v, want %v", line, got, want)
		}
	}
}

// The optimizer cooldown is the same dedup window and breaker cooldown as
// llm.cooldown_seconds (the optimizer client inherits it when 0), not a
// minimum gap between requests.
func TestOptimizerCooldownDocDescribesBreaker(t *testing.T) {
	field, ok := DescribeField(nil, "llm.optimizer_llm.cooldown_seconds")
	if !ok {
		t.Fatal("llm.optimizer_llm.cooldown_seconds is not a config key")
	}
	if strings.Contains(field.Doc, "Minimum seconds between") {
		t.Errorf("doc still describes a request gap: %q", field.Doc)
	}
	for _, want := range []string{"identical request", "breaker", "llm.cooldown_seconds"} {
		if !strings.Contains(field.Doc, want) {
			t.Errorf("doc %q does not mention %q", field.Doc, want)
		}
	}
}
