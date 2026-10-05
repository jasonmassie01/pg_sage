package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Five-minute time to value: docs/quickstart.md must match the product.
// Every config key, CLI flag, environment variable, metric, API path and
// UI label it names has to exist, so the docs cannot drift from the UI.

const quickstartPath = "../../../docs/quickstart.md"

var (
	backtickToken = regexp.MustCompile("`([^`\\s]+)`")
	dottedKey     = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)
	cliFlag       = regexp.MustCompile(`(?:^|\s)(--[a-z][a-z-]*)`)
	envVar        = regexp.MustCompile(`\bSAGE_[A-Z0-9_]+\b`)
	metricName    = regexp.MustCompile(`\bpg_sage_[a-z0-9_]+\b`)
	apiPath       = regexp.MustCompile(`/api/v1/[a-z0-9_/-]*[a-z0-9_]`)
	uiLabel       = regexp.MustCompile(`\*\*"([^"]+)"\*\*`)
	// Dotted tokens that are not pg_sage config keys: sage tables,
	// PostgreSQL settings and file names.
	notConfigKey = regexp.MustCompile(`^(sage\.|pg_stat_statements\.|auto_explain\.|` +
		`shared_preload_libraries)|\.(ya?ml|md|sql|json|sh)$`)
)

func readQuickstart(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(quickstartPath)
	if err != nil {
		t.Fatalf("read quickstart: %v", err)
	}
	return string(raw)
}

func TestQuickstartConfigKeysExist(t *testing.T) {
	doc := readQuickstart(t)
	checked := 0
	for _, m := range backtickToken.FindAllStringSubmatch(doc, -1) {
		token := strings.TrimSuffix(m[1], ":")
		if !dottedKey.MatchString(token) || notConfigKey.MatchString(token) {
			continue
		}
		checked++
		if !yamlPathExists(reflect.TypeOf(Config{}), strings.Split(token, ".")) {
			t.Errorf("docs/quickstart.md names config key %q, which does not exist", token)
		}
	}
	if checked < 2 {
		t.Fatalf("only %d config keys found; the quickstart must name trust.level and "+
			"collector.interval_seconds", checked)
	}
}

func TestQuickstartCLIFlagsExist(t *testing.T) {
	doc := readQuickstart(t)
	checked := 0
	for _, line := range strings.Split(doc, "\n") {
		if !strings.Contains(line, "./pg_sage") && !strings.Contains(line, "pg_sage_sidecar ") {
			continue
		}
		for _, m := range cliFlag.FindAllStringSubmatch(line, -1) {
			checked++
			_, err := Load([]string{m[1], "1"})
			if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
				t.Errorf("docs/quickstart.md uses CLI flag %s, which does not exist", m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("the quickstart shows no binary command line")
	}
}

func TestQuickstartEnvVarsMetricsPathsAndLabelsExist(t *testing.T) {
	doc := readQuickstart(t)
	goSrc := sourceText(t, "../../cmd/pg_sage_sidecar", "../../internal/config",
		"../../internal/api", "../../internal/onboarding")
	uiSrc := sourceText(t, "../../web/src", "../../internal/onboarding")
	checks := []struct {
		kind string
		re   *regexp.Regexp
		src  string
		want func(string) string
	}{
		{"environment variable", envVar, goSrc, func(s string) string { return `"` + s + `"` }},
		{"metric", metricName, goSrc, func(s string) string { return s }},
		{"API path", apiPath, goSrc, func(s string) string { return s }},
	}
	for _, c := range checks {
		found := c.re.FindAllString(doc, -1)
		if len(found) == 0 {
			t.Errorf("the quickstart names no %s", c.kind)
		}
		for _, token := range found {
			if !strings.Contains(c.src, c.want(token)) {
				t.Errorf("docs/quickstart.md names %s %s, which the code does not define",
					c.kind, token)
			}
		}
	}
	labels := uiLabel.FindAllStringSubmatch(doc, -1)
	if len(labels) < 3 {
		t.Fatalf("the quickstart quotes %d UI labels, want the checklist's", len(labels))
	}
	for _, m := range labels {
		if !strings.Contains(uiSrc, m[1]) {
			t.Errorf("docs/quickstart.md quotes UI label %q, which the UI does not show", m[1])
		}
	}
}

func TestYAMLPathExists(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for path, want := range map[string]bool{
		"trust.level": true, "collector.interval_seconds": true, "llm.api_key": true,
		"trust.levle": false, "nope.x": false, "trust": true, "trust.level.extra": false,
	} {
		if got := yamlPathExists(typ, strings.Split(path, ".")); got != want {
			t.Errorf("yamlPathExists(%q) = %v, want %v", path, got, want)
		}
	}
}

// yamlPathExists reports whether parts name a field path through yaml tags.
func yamlPathExists(typ reflect.Type, parts []string) bool {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if len(parts) == 0 {
		return true
	}
	if typ.Kind() == reflect.Map {
		return true // free-form keys
	}
	if typ.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == parts[0] {
			return yamlPathExists(f.Type, parts[1:])
		}
		if name == "" && f.Anonymous && yamlPathExists(f.Type, parts) {
			return true
		}
	}
	return false
}

// sourceText concatenates the non-test sources (.go, .js, .jsx) of dirs.
func sourceText(t *testing.T, dirs ...string) string {
	t.Helper()
	var b strings.Builder
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			ext := filepath.Ext(path)
			if d.IsDir() || strings.Contains(path, "_test.") || strings.Contains(path, ".test.") ||
				(ext != ".go" && ext != ".js" && ext != ".jsx") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			b.Write(raw)
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read %s: %v", dir, err)
		}
	}
	return b.String()
}
