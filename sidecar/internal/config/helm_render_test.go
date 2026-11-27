package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

// helmRender renders deploy/helm/pg-sage with text/template and the subset
// of Helm's functions the chart uses (market-12). It lets the chart be
// checked without the helm binary; CI also runs helm lint, helm template
// and kubeconform. A function the chart uses but this file lacks fails the
// parse, so the subset cannot drift silently.

var chartDir = filepath.Join("..", "..", "..", "deploy", "helm", "pg-sage")

type chartRelease struct {
	Name, Namespace, Service string
}

func readYAMLMap(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// mergeValues overlays src onto dst, recursing into maps as Helm does.
func mergeValues(dst, src map[string]any) map[string]any {
	for k, v := range src {
		sm, sok := v.(map[string]any)
		dm, dok := dst[k].(map[string]any)
		if sok && dok {
			dst[k] = mergeValues(dm, sm)
			continue
		}
		dst[k] = v
	}
	return dst
}

// renderChart returns each template's output (keyed by file name) for the
// chart's values with overrides applied, or the first render error.
func renderChart(t *testing.T, overrides map[string]any) (map[string]string, error) {
	t.Helper()
	meta := readYAMLMap(t, filepath.Join(chartDir, "Chart.yaml"))
	values := mergeValues(readYAMLMap(t, filepath.Join(chartDir, "values.yaml")),
		deepCopyValue(overrides).(map[string]any))
	data := map[string]any{"Values": values,
		"Chart": map[string]any{"Name": meta["name"], "Version": meta["version"],
			"AppVersion": meta["appVersion"]},
		"Release": chartRelease{Name: "demo", Namespace: "db", Service: "Helm"}}
	root := template.New("chart").Option("missingkey=zero")
	root.Funcs(helmFuncs(root))
	files, err := filepath.Glob(filepath.Join(chartDir, "templates", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("chart templates: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := root.New(filepath.Base(f)).Parse(string(raw)); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
	}
	out := map[string]string{}
	for _, f := range files {
		name := filepath.Base(f)
		if strings.HasPrefix(name, "_") {
			continue
		}
		var buf bytes.Buffer
		if err := root.ExecuteTemplate(&buf, name, data); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = strings.ReplaceAll(buf.String(), "<no value>", "")
	}
	return out, nil
}

func helmFuncs(root *template.Template) template.FuncMap {
	return template.FuncMap{
		"include": func(name string, data any) (string, error) {
			var buf bytes.Buffer
			err := root.ExecuteTemplate(&buf, name, data)
			return buf.String(), err
		},
		"default": func(d any, given ...any) any {
			if len(given) == 0 || emptyValue(given[0]) {
				return d
			}
			return given[0]
		},
		"trunc": func(n int, s string) string {
			if len(s) > n {
				return s[:n]
			}
			return s
		},
		"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
		"contains":   func(sub, s string) bool { return strings.Contains(s, sub) },
		"quote":      func(v any) string { return fmt.Sprintf("%q", fmt.Sprint(v)) },
		"toString":   func(v any) string { return fmt.Sprint(v) },
		"toYaml":     toYAML,
		"indent":     indentText,
		"nindent":    func(n int, s string) string { return "\n" + indentText(n, s) },
		"fail":       func(msg string) (string, error) { return "", errors.New(msg) },
		"ternary": func(a, b any, cond bool) any {
			if cond {
				return a
			}
			return b
		},
		"list":     func(v ...any) []any { return append([]any{}, v...) },
		"append":   func(l []any, v any) []any { return append(append([]any{}, l...), v) },
		"uniq":     uniqValues,
		"dict":     dictValue,
		"set":      func(d map[string]any, k string, v any) map[string]any { d[k] = v; return d },
		"get":      func(d map[string]any, k string) any { return d[k] },
		"hasKey":   func(d map[string]any, k string) bool { _, ok := d[k]; return ok },
		"deepCopy": deepCopyValue,
	}
}

func emptyValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int64, reflect.Float64:
		return rv.IsZero()
	}
	return false
}

func toYAML(v any) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), enc.Close()
}

func indentText(n int, s string) string {
	pad := strings.Repeat(" ", n)
	return pad + strings.ReplaceAll(s, "\n", "\n"+pad)
}

func uniqValues(l []any) []any {
	out := []any{}
	for _, v := range l {
		if !containsValue(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func containsValue(l []any, v any) bool {
	for _, x := range l {
		if reflect.DeepEqual(x, v) {
			return true
		}
	}
	return false
}

func dictValue(kv ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[fmt.Sprint(kv[i])] = kv[i+1]
	}
	return out
}

func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = deepCopyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = deepCopyValue(val)
		}
		return out
	}
	return v
}

// renderedDocs parses one rendered template into its YAML documents.
func renderedDocs(t *testing.T, name, text string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(strings.NewReader(text))
	for {
		doc := map[string]any{}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("%s does not parse as YAML: %v\n%s", name, err, text)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
}
