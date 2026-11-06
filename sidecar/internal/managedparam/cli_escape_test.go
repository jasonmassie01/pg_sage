package managedparam

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// No concurrency or integration test applies here: the command builders
// are pure string functions over their arguments, and the worker DB tests
// already cover how proposals are persisted.

// shellMeta are the characters a POSIX shell interprets when unquoted.
const shellMeta = "$`;&|<>()*?!\\\"'{}[]#~"

// shlexSplit splits a command line as a POSIX shell would (single quotes,
// double quotes, backslash escapes) and fails when a character the shell
// interprets appears unquoted: a generated command must never rely on one.
func shlexSplit(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				out, inWord = append(out, cur.String()), false
				cur.Reset()
			}
		case c == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote in %q", line)
			}
			cur.WriteString(line[i+1 : i+1+end])
			i, inWord = i+1+end, true
		case c == '\\' && i+1 < len(line):
			cur.WriteByte(line[i+1])
			i, inWord = i+1, true
		case strings.IndexByte(shellMeta, c) >= 0 && !placeholderByte(line, i):
			return nil, fmt.Errorf("unquoted shell metacharacter %q at %d in %q", c, i, line)
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

// placeholderByte allows the <placeholder> markers of unresolved targets.
func placeholderByte(line string, i int) bool {
	return line[i] == '<' || line[i] == '>'
}

// parseGcloudDict parses a gcloud dictionary flag value, honouring the
// ^DELIM^ alternate-delimiter syntax of `gcloud topic escaping`.
func parseGcloudDict(t *testing.T, arg string) map[string]string {
	t.Helper()
	delim := ","
	if strings.HasPrefix(arg, "^") {
		end := strings.IndexByte(arg[1:], '^')
		if end <= 0 {
			t.Fatalf("bad ^delimiter^ prefix in %q", arg)
		}
		delim, arg = arg[1:1+end], arg[2+end:]
	}
	out := map[string]string{}
	for _, item := range strings.Split(arg, delim) {
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" {
			t.Fatalf("item %q of %q is not key=value", item, arg)
		}
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate key %q in %q", k, arg)
		}
		out[k] = v
	}
	return out
}

// databaseFlags returns the parsed --database-flags of a gcloud command.
func databaseFlags(t *testing.T, cmd string) map[string]string {
	t.Helper()
	args, err := shlexSplit(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) < 4 || strings.Join(args[:4], " ") != "gcloud sql instances patch" {
		t.Fatalf("not a gcloud patch command: %q", cmd)
	}
	var found []map[string]string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--database-flags="); ok {
			found = append(found, parseGcloudDict(t, v))
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one --database-flags argument in %q, got %d", cmd, len(found))
	}
	return found[0]
}

func assertFlags(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s flags = %v, want %v", what, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s flag %s = %q, want %q (all: %v)", what, k, got[k], v, got)
		}
	}
}

func flagTarget(flags map[string]string) Target {
	return Target{Provider: "cloud-sql", Project: "proj-1", InstanceID: "main", Known: true,
		FlagsKnown: true, Flags: flags}
}

// notRunnable is true when a command is empty or only comment lines.
func notRunnable(cmd string) bool {
	for _, line := range strings.Split(cmd, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			return false
		}
	}
	return true
}

func TestCloudSQLCommaValueRoundTrips(t *testing.T) {
	current := map[string]string{"pgaudit.log": "read,write", "work_mem": "65536"}
	p, err := Build(intent(t, "cloud-sql", "random_page_cost", "1.1"), flagTarget(current), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(p.Blockers) != 0 {
		t.Fatalf("a comma value must be escaped, not blocked: %v", p.Blockers)
	}
	assertFlags(t, "forward", databaseFlags(t, p.CLI), map[string]string{
		"pgaudit.log": "read,write", "work_mem": "65536", "random_page_cost": "1.1"})
	assertFlags(t, "rollback", databaseFlags(t, p.Rollback.CLI), current)
	if !strings.Contains(p.CLI, "^@^") {
		t.Fatalf("the first free delimiter is @: %q", p.CLI)
	}
}

func TestCloudSQLDelimiterSkipsOnesInValues(t *testing.T) {
	current := map[string]string{"cloudsql.x": "a,b@c", "log_timezone": "Etc/UTC:x|y"}
	p, err := Build(intent(t, "cloud-sql", "work_mem", "64MB"), flagTarget(current), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(p.CLI, "^~^") || len(p.Blockers) != 0 {
		t.Fatalf("@ : | occur in values, so ~ is the delimiter: %q %v", p.CLI, p.Blockers)
	}
	assertFlags(t, "forward", databaseFlags(t, p.CLI), map[string]string{
		"cloudsql.x": "a,b@c", "log_timezone": "Etc/UTC:x|y", "work_mem": "65536"})
}

func TestCloudSQLPlainValuesAreQuotedWithoutDelimiter(t *testing.T) {
	p, err := Build(intent(t, "cloud-sql", "work_mem", "64MB"),
		flagTarget(map[string]string{"log_min_duration_statement": "500"}), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "gcloud sql instances patch main --project=proj-1 " +
		"--database-flags='log_min_duration_statement=500,work_mem=65536'"
	if p.CLI != want {
		t.Fatalf("CLI = %q, want %q", p.CLI, want)
	}
}

func TestCloudSQLUnsafeProviderValuesAreBlocked(t *testing.T) {
	for _, bad := range []string{"$(id)", "a;b", "a b", "it's", "`id`", "a\"b", `a\b`,
		"a\nb", "a\x00b", strings.Repeat("a", 257)} {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			current := map[string]string{"log_line_prefix": bad, "work_mem": "65536"}
			p, err := Build(intent(t, "cloud-sql", "random_page_cost", "1.1"),
				flagTarget(current), nil)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !notRunnable(p.CLI) || !notRunnable(p.Rollback.CLI) {
				t.Fatalf("no runnable command may carry %q: cli=%q rollback=%q", bad, p.CLI,
					p.Rollback.CLI)
			}
			if strings.Contains(p.CLI, bad) || strings.Contains(p.Rollback.CLI, bad) {
				t.Fatalf("the unsafe value must not be echoed into a command: %q", p.CLI)
			}
			if len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], "log_line_prefix") {
				t.Fatalf("want one blocker naming the flag, got %v", p.Blockers)
			}
			assertApprovalOnly(t, p)
		})
	}
}

func TestCloudSQLUnsafeProviderFlagNameIsBlocked(t *testing.T) {
	current := map[string]string{"x;rm -rf /": "1", "work_mem": "65536"}
	p, err := Build(intent(t, "cloud-sql", "random_page_cost", "1.1"), flagTarget(current), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !notRunnable(p.CLI) || !notRunnable(p.Rollback.CLI) || len(p.Blockers) != 1 ||
		strings.Contains(p.CLI, "rm -rf") {
		t.Fatalf("an invalid flag name must block: cli=%q blockers=%v", p.CLI, p.Blockers)
	}
}

func TestCloudSQLEveryDelimiterInValueIsBlocked(t *testing.T) {
	current := map[string]string{"cloudsql.x": "a,@:|~", "work_mem": "65536"}
	p, err := Build(intent(t, "cloud-sql", "random_page_cost", "1.1"), flagTarget(current), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !notRunnable(p.CLI) || !notRunnable(p.Rollback.CLI) || len(p.Blockers) != 1 ||
		!strings.Contains(p.Blockers[0], "cloudsql.x") {
		t.Fatalf("no free delimiter must block: cli=%q blockers=%v", p.CLI, p.Blockers)
	}
}

func TestCloudSQLLongestAllowedValue(t *testing.T) {
	long := strings.Repeat("a", 256)
	p, err := Build(intent(t, "cloud-sql", "random_page_cost", "1.1"),
		flagTarget(map[string]string{"cloudsql.x": long}), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(p.Blockers) != 0 {
		t.Fatalf("a 256-character value is allowed: %v", p.Blockers)
	}
	assertFlags(t, "forward", databaseFlags(t, p.CLI),
		map[string]string{"cloudsql.x": long, "random_page_cost": "1.1"})
}

// Unknown flags: --database-flags replaces the whole list and
// --clear-database-flags removes it, so neither may appear in a command
// built without the current flags.
func TestCloudSQLUnknownFlagsGiveTemplateOnly(t *testing.T) {
	for _, target := range []Target{
		{Provider: "cloud-sql", Project: "proj-1", InstanceID: "main"},
		{Provider: "cloud-sql", Project: "proj-1", InstanceID: "main", Known: true,
			Flags: map[string]string{"work_mem": "65536"}},
		{Provider: "cloud-sql", Unresolved: "no credentials"},
	} {
		p, err := Build(intent(t, "cloud-sql", "work_mem", "64MB"), target, nil)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		for name, cmd := range map[string]string{"cli": p.CLI, "rollback": p.Rollback.CLI} {
			if strings.Contains(cmd, "--database-flags=") ||
				strings.Contains(cmd, "--clear-database-flags") || !notRunnable(cmd) ||
				!strings.Contains(cmd, "gcloud sql instances describe") {
				t.Fatalf("%s must be a fill-in template: %q", name, cmd)
			}
		}
		if !strings.Contains(p.CLI, "work_mem=65536") {
			t.Fatalf("the template must name the change: %q", p.CLI)
		}
		if len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], "replaces") {
			t.Fatalf("blockers = %v", p.Blockers)
		}
	}
}

func TestCloudSQLClearFlagsOnlyWhenKnownEmpty(t *testing.T) {
	p, err := Build(intent(t, "cloud-sql", "work_mem", "64MB"),
		flagTarget(map[string]string{}), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Rollback.CLI != "gcloud sql instances patch main --project=proj-1 "+
		"--clear-database-flags" {
		t.Fatalf("known-empty rollback = %q", p.Rollback.CLI)
	}
	assertFlags(t, "forward", databaseFlags(t, p.CLI), map[string]string{"work_mem": "65536"})
	nilFlags := flagTarget(nil)
	p, err = Build(intent(t, "cloud-sql", "work_mem", "64MB"), nilFlags, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(p.Rollback.CLI, "--clear-database-flags") {
		t.Fatalf("known flags with a nil map are known-empty: %q", p.Rollback.CLI)
	}
	others := []Target{flagTarget(map[string]string{"work_mem": "1024"}),
		flagTarget(map[string]string{"jit": "off"})}
	for _, target := range others {
		p, err := Build(intent(t, "cloud-sql", "work_mem", "64MB"), target, nil)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if strings.Contains(p.CLI+p.Rollback.CLI, "--clear-database-flags") {
			t.Fatalf("flags %v must never clear: %q / %q", target.Flags, p.CLI,
				p.Rollback.CLI)
		}
	}
}

type rdsParam struct {
	ParameterName  string
	ParameterValue string
	ApplyMethod    string
}

// rdsParameters returns the decoded --parameters JSON of an aws command.
func rdsParameters(t *testing.T, cmd string) []rdsParam {
	t.Helper()
	args, err := shlexSplit(strings.SplitN(cmd, "\n", 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range args {
		if a == "--parameters" && i+1 < len(args) {
			var out []rdsParam
			if err := json.Unmarshal([]byte(args[i+1]), &out); err != nil {
				t.Fatalf("--parameters %q is not JSON: %v", args[i+1], err)
			}
			return out
		}
	}
	t.Fatalf("no --parameters in %q", cmd)
	return nil
}

func preloadTarget(value string) Target {
	target := rdsTarget()
	target.Params["shared_preload_libraries"] = GroupParam{Value: value, Source: "user",
		ApplyType: "static", Modifiable: true}
	return target
}

func TestRDSListValueRollbackIsJSON(t *testing.T) {
	prior := "pg_stat_statements,pg_hint_plan"
	p, err := Build(intent(t, "rds", "shared_preload_libraries", "pg_stat_statements"),
		preloadTarget(prior), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	back := rdsParameters(t, p.Rollback.CLI)
	if len(back) != 1 || back[0] != (rdsParam{"shared_preload_libraries", prior,
		ApplyPendingReboot}) {
		t.Fatalf("rollback parameters = %+v", back)
	}
	fwd := rdsParameters(t, p.CLI)
	if len(fwd) != 1 || fwd[0] != (rdsParam{"shared_preload_libraries",
		"pg_stat_statements", ApplyPendingReboot}) {
		t.Fatalf("forward parameters = %+v", fwd)
	}
	if len(p.Blockers) != 0 {
		t.Fatalf("blockers = %v", p.Blockers)
	}
}

func TestRDSResetIsJSON(t *testing.T) {
	p, err := Build(intent(t, "rds", "shared_buffers", "4GB"), rdsTarget(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	back := rdsParameters(t, p.Rollback.CLI)
	if !p.Rollback.Reset || len(back) != 1 || back[0] != (rdsParam{ParameterName: "shared_buffers",
		ApplyMethod: ApplyPendingReboot}) ||
		!strings.HasPrefix(p.Rollback.CLI, "aws rds reset-db-parameter-group ") {
		t.Fatalf("reset rollback = %+v", p.Rollback)
	}
}

func TestRDSUnsafeRollbackValueIsBlocked(t *testing.T) {
	for _, bad := range []string{"it's", "$(id)", "a b", "a\nb"} {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			p, err := Build(intent(t, "rds", "shared_preload_libraries", "pg_stat_statements"),
				preloadTarget(bad), nil)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !notRunnable(p.Rollback.CLI) || strings.Contains(p.Rollback.CLI, bad) {
				t.Fatalf("rollback must not carry %q: %q", bad, p.Rollback.CLI)
			}
			if len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0],
				"shared_preload_libraries") {
				t.Fatalf("want one blocker naming the parameter, got %v", p.Blockers)
			}
			if p.Rollback.Value != "" {
				t.Fatalf("an unsafe value must not be stored as the rollback value: %q",
					p.Rollback.Value)
			}
			assertApprovalOnly(t, p)
		})
	}
}

func TestShlexSplitRejectsUnquotedMeta(t *testing.T) {
	if _, err := shlexSplit("gcloud x --database-flags=a=$(id)"); err == nil {
		t.Fatal("the test splitter must reject an unquoted $")
	}
	got, err := shlexSplit("a 'b c' d'e'")
	if err != nil || len(got) != 3 || got[1] != "b c" || got[2] != "de" {
		t.Fatalf("split = %q, %v", got, err)
	}
}

func TestShellQuoteRoundTrips(t *testing.T) {
	for _, v := range []string{"", "plain", "it's", "''", "$(id); rm -rf / `x`", "a b	c"} {
		got, err := shlexSplit("cmd " + shellQuote(v) + " end")
		if err != nil || len(got) != 3 || got[1] != v {
			t.Fatalf("shellQuote(%q) split = %q, %v", v, got, err)
		}
	}
}

// An unsafe flag withholds both Cloud SQL commands, but the changed
// flag's own prior value is cleared from the rollback only when it is
// the unsafe one (as on RDS).
func TestCloudSQLBlockedRollbackKeepsSafePriorValue(t *testing.T) {
	current := map[string]string{"log_line_prefix": "a b", "work_mem": "1024"}
	p, err := Build(intent(t, "cloud-sql", "work_mem", "64MB"), flagTarget(current), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !notRunnable(p.CLI) || len(p.Blockers) != 1 || p.Rollback.Reset ||
		p.Rollback.Value != "1024" {
		t.Fatalf("safe prior must stay the rollback value: cli=%q rollback=%+v blockers=%v",
			p.CLI, p.Rollback, p.Blockers)
	}
	current = map[string]string{"log_line_prefix": "a b", "work_mem": "1024"}
	p, err = Build(intent(t, "cloud-sql", "log_line_prefix", "x"), flagTarget(current), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !notRunnable(p.Rollback.CLI) || p.Rollback.Value != "" || len(p.Blockers) != 1 {
		t.Fatalf("an unsafe prior must not be the rollback value: %+v %v", p.Rollback,
			p.Blockers)
	}
}
