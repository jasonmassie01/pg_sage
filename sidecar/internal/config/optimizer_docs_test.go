package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// indexes-11: every llm.optimizer key docs/configuration.md documents (in
// the YAML example under "optimizer:" and in the parameter table) must map
// to an OptimizerConfig field that code outside internal/config reads. A
// documented key with no reader is a promise the sidecar does not keep.

var (
	docsTableOptimizerKey = regexp.MustCompile("\\| `llm\\.optimizer\\.([a-z0-9_.]+)` \\|")
	docsYAMLKey           = regexp.MustCompile(`^(\s*)([a-z0-9_]+):`)
)

// optimizerDocKeys returns the dotted llm.optimizer sub-paths body names.
func optimizerDocKeys(body string) []string {
	seen := map[string]bool{}
	for _, m := range docsTableOptimizerKey.FindAllStringSubmatch(body, -1) {
		seen[m[1]] = true
	}
	for _, path := range yamlOptimizerKeys(strings.Split(body, "\n")) {
		seen[path] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// yamlOptimizerKeys walks each indented "optimizer:" block of lines and
// returns its keys as dotted paths relative to the block.
func yamlOptimizerKeys(lines []string) []string {
	var paths []string
	for i := 0; i < len(lines); i++ {
		m := docsYAMLKey.FindStringSubmatch(lines[i])
		if m == nil || m[2] != "optimizer" || m[1] == "" {
			continue
		}
		base := len(m[1])
		type level struct {
			indent int
			key    string
		}
		var stack []level
		for i++; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "" ||
				strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
				continue
			}
			k := docsYAMLKey.FindStringSubmatch(lines[i])
			if k == nil || len(k[1]) <= base {
				i--
				break
			}
			for len(stack) > 0 && stack[len(stack)-1].indent >= len(k[1]) {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, level{len(k[1]), k[2]})
			parts := make([]string, len(stack))
			for j, l := range stack {
				parts[j] = l.key
			}
			paths = append(paths, strings.Join(parts, "."))
		}
	}
	return paths
}

// optimizerFieldChain resolves a dotted yaml path to Go field names.
func optimizerFieldChain(path string) ([]string, bool) {
	t := reflect.TypeOf(OptimizerConfig{})
	var chain []string
	for _, seg := range strings.Split(path, ".") {
		if t.Kind() != reflect.Struct {
			return nil, false
		}
		found := false
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if strings.Split(f.Tag.Get("yaml"), ",")[0] == seg {
				chain, t, found = append(chain, f.Name), f.Type, true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return chain, true
}

// sidecarSourcesOutsideConfig concatenates the non-test Go sources of the
// sidecar module outside internal/config and the config metadata generator
// (which reflects over every field and so reads none of them).
func sidecarSourcesOutsideConfig(t *testing.T) string {
	t.Helper()
	root := filepath.Join(wave5RepoRoot(t), "sidecar")
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root))
		if d.IsDir() && (rel == "/internal/config" || rel == "/cmd/gen_config_meta" ||
			rel == "/web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		b.Write(body)
		b.WriteByte('\n')
		return err
	})
	if err != nil {
		t.Fatalf("read sidecar sources: %v", err)
	}
	return b.String()
}

func TestOptimizerDocsKeysHaveReaders(t *testing.T) {
	body := wave5ReadFile(t, filepath.Join(wave5RepoRoot(t), "docs", "configuration.md"))
	keys := optimizerDocKeys(body)
	if len(keys) < 5 {
		t.Fatalf("found only %d llm.optimizer keys in docs (%v): the parser is broken",
			len(keys), keys)
	}
	sources := sidecarSourcesOutsideConfig(t)
	for _, key := range keys {
		chain, ok := optimizerFieldChain(key)
		if !ok {
			t.Errorf("docs/configuration.md documents llm.optimizer.%s, which is not "+
				"an OptimizerConfig field", key)
			continue
		}
		for _, field := range chain {
			if !regexp.MustCompile(`\.` + field + `\b`).MatchString(sources) {
				t.Errorf("docs/configuration.md documents llm.optimizer.%s, but no code "+
					"outside internal/config reads %s", key, field)
			}
		}
	}
}

func TestOptimizerDocKeysParser(t *testing.T) {
	body := "llm:\n  optimizer:\n    enabled: true   # x\n\n    # comment: no\n" +
		"    rejection_memory:\n      max_age_days: 7\n    min_query_calls: 1\n" +
		"  optimizer_llm:\n    model: x\noptimizer:\n  top: 1\n" +
		"| `llm.optimizer.max_new_per_table` | `3` | d |\n"
	got := optimizerDocKeys(body)
	want := []string{"enabled", "max_new_per_table", "min_query_calls",
		"rejection_memory", "rejection_memory.max_age_days"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("optimizerDocKeys = %v, want %v", got, want)
	}
	if _, ok := optimizerFieldChain("rejection_memory.max_age_days"); !ok {
		t.Fatal("nested key did not resolve")
	}
	for _, bad := range []string{"", "nope", "enabled.deeper", "rejection_memory.nope"} {
		if _, ok := optimizerFieldChain(bad); ok {
			t.Fatalf("optimizerFieldChain(%q) resolved; want not found", bad)
		}
	}
}

// No concurrency, state or database tests: a static check of shipped docs.
