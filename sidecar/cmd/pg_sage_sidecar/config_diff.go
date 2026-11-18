package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"text/tabwriter"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// `config diff <file>` (cli-01b) compares a file with the running
// sidecar's effective settings (GET /api/v1/config/global, the keys the
// Settings page shows) and prints each key where they part: the file's
// value, the effective value and where the effective value comes from. A
// stored override is always printed, even one equal to the file, because
// it wins over every later edit of the file. Secrets print as <secret>.

// Sources of an effective value as diff names them.
const (
	diffSourceOverride   = "stored override"
	diffSourceDBOverride = "database override"
	diffSourceEnv        = "env" // a SAGE_* variable, a flag or another file
	diffSecretShown      = "<secret>"
)

// configDiffRow is one key where the file and the running config part.
type configDiffRow struct {
	key, file, effective, source string
}

func runConfigDiff(ctx context.Context, args []string, env cliEnv) int {
	fs := newFlagSet("config diff")
	var f cliFlags
	fs.StringVar(&f.url, "url", "", "the sidecar's API (default $SAGE_API_URL)")
	file, code, done := parseFileArgs(fs, args, configDiffUsage, env)
	if done {
		return code
	}
	if isDirectory(file) {
		return diffDirectory(ctx, file, f, env) // a policy.directory (cli-02a)
	}
	check, err := config.CheckFile(file)
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "config diff: %v\n", err)
		return configExitError
	}
	if len(check.Problems) > 0 {
		printProblems(env, file, check.Problems)
		_, _ = fmt.Fprintln(env.stderr, "config diff: fix the file first (config validate)")
		return configExitError
	}
	s, code := connect(fs.Name(), f, env, configExitAuth)
	if s == nil {
		return code
	}
	ans, err := s.api.send(ctx, http.MethodGet, "/api/v1/config/global", nil, nil)
	if err != nil {
		return s.fail(err, configExitAuth)
	}
	rows, err := configDiffRows(check.Config, ans.body)
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "config diff: %v\n", err)
		return configExitError
	}
	return printConfigDiff(env, file, rows)
}

func printConfigDiff(env cliEnv, file string, rows []configDiffRow) int {
	if len(rows) == 0 {
		_, _ = fmt.Fprintf(env.stdout, "no differences: the running config matches %s\n", file)
		return configExitOK
	}
	tw := tabwriter.NewWriter(env.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KEY\tFILE\tEFFECTIVE\tSOURCE")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.key, r.file, r.effective, r.source)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(env.stdout, "%d key(s) differ from %s\n", len(rows), file)
	return configExitProblem
}

// configDiffRows compares the file's settings with the sidecar's answer
// ({"config": {key: {"value", "source"}}}), sorted by key. A key this CLI
// does not know is skipped: it cannot be in the file.
func configDiffRows(fileCfg *config.Config, body map[string]any) ([]configDiffRow, error) {
	effective, ok := body["config"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the sidecar's answer has no config map")
	}
	fileValues, err := normalizedReadModel(fileCfg)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(effective))
	for k := range effective {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var rows []configDiffRow
	for _, key := range keys {
		entry, _ := effective[key].(map[string]any)
		fileEntry, known := fileValues[key].(map[string]any)
		if entry == nil || !known {
			continue
		}
		if row, differs := diffRow(key, fileEntry["value"], entry); differs {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// diffRow compares one key; differs is false when the values agree and
// no override sets the effective one.
func diffRow(key string, fileValue any, entry map[string]any) (configDiffRow, bool) {
	effValue := entry["value"]
	source, _ := entry["source"].(string)
	equal := reflect.DeepEqual(fileValue, effValue)
	row := configDiffRow{key: key, file: diffValue(key, fileValue),
		effective: diffValue(key, effValue)}
	switch source {
	case "override":
		row.source = diffSourceOverride
	case "db_override":
		row.source = diffSourceDBOverride
	default:
		if equal {
			return row, false
		}
		row.source = diffSourceEnv
	}
	if secretConfigKey(key) {
		row.file, row.effective = diffSecretShown, diffSecretShown
		if !equal {
			row.effective = diffSecretShown + " (differs)"
		}
	}
	return row, true
}

// normalizedReadModel is the file's read model with JSON's types, so it
// compares with a decoded answer (every number a float64).
func normalizedReadModel(cfg *config.Config) (map[string]any, error) {
	raw, err := json.Marshal(store.ConfigReadModel(cfg))
	if err != nil {
		return nil, fmt.Errorf("read the file's settings: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("read the file's settings: %w", err)
	}
	return out, nil
}

func secretConfigKey(key string) bool {
	return store.IsSecretConfigKey(key) || config.LooksSecretKey(key)
}

// diffValue prints one value; a secret never prints.
func diffValue(key string, v any) string {
	if secretConfigKey(key) {
		return diffSecretShown
	}
	switch t := v.(type) {
	case nil:
		return "(unset)"
	case string:
		if t == "" {
			return `""`
		}
		return t
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
