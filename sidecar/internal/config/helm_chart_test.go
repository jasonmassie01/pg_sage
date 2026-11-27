package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// market-12: the Helm chart's rendered objects (helm_render_test.go) and
// its config.yaml, loaded by pg_sage's own loader.

// No concurrent access tests: rendering is a pure function of the chart.

func mustRender(t *testing.T, overrides map[string]any) map[string]string {
	t.Helper()
	out, err := renderChart(t, overrides)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

func onlyDoc(t *testing.T, out map[string]string, file string) map[string]any {
	t.Helper()
	docs := renderedDocs(t, file, out[file])
	if len(docs) != 1 {
		t.Fatalf("%s rendered %d objects, want 1:\n%s", file, len(docs), out[file])
	}
	return docs[0]
}

// at walks a rendered object by map keys and list indexes.
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v: %T is not a map at %q", path, v, k)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("%v: no index %d in %v", path, k, v)
			}
			v = l[k]
		}
	}
	return v
}

// CHECK-01 (local part): the image tag is the chart appVersion, never latest.
func TestHelmChartDefaultDeployment(t *testing.T) {
	out := mustRender(t, nil)
	meta := readYAMLMap(t, filepath.Join(chartDir, "Chart.yaml"))
	spec := at(t, onlyDoc(t, out, "deployment.yaml"), "spec")
	pod := at(t, spec, "template", "spec")
	image := at(t, pod, "containers", 0, "image")
	if image != "ghcr.io/jasonmassie01/pg_sage:"+meta["appVersion"].(string) ||
		strings.HasSuffix(image.(string), ":latest") {
		t.Fatalf("image = %v, want the appVersion %v", image, meta["appVersion"])
	}
	if at(t, spec, "replicas") != 1 || at(t, spec, "strategy", "type") != "Recreate" {
		t.Fatalf("replicas %v strategy %v", at(t, spec, "replicas"), at(t, spec, "strategy"))
	}
	sc := at(t, pod, "containers", 0, "securityContext")
	if at(t, pod, "securityContext", "runAsNonRoot") != true ||
		at(t, sc, "readOnlyRootFilesystem") != true ||
		at(t, sc, "allowPrivilegeEscalation") != false ||
		!reflect.DeepEqual(at(t, sc, "capabilities", "drop"), []any{"ALL"}) {
		t.Fatalf("security contexts: pod %v container %v", at(t, pod, "securityContext"), sc)
	}
	if at(t, pod, "automountServiceAccountToken") != false {
		t.Fatal("without CNPG the service-account token is not mounted")
	}
	if at(t, pod, "containers", 0, "readinessProbe", "httpGet", "scheme") != "HTTP" {
		t.Fatalf("readiness probe = %v", at(t, pod, "containers", 0, "readinessProbe"))
	}
	if docs := renderedDocs(t, "role.yaml", out["role.yaml"]); len(docs) != 0 {
		t.Fatalf("no Role without a Cluster: %v", docs)
	}
	for _, f := range []string{"servicemonitor.yaml", "networkpolicy.yaml"} {
		if len(renderedDocs(t, f, out[f])) != 0 {
			t.Fatalf("%s is optional and off by default", f)
		}
	}
}

func TestHelmChartRefusesLatest(t *testing.T) {
	_, err := renderChart(t, map[string]any{"image": map[string]any{"tag": "latest"}})
	if err == nil || !strings.Contains(err.Error(), "latest is not allowed") {
		t.Fatalf("err = %v", err)
	}
	out := mustRender(t, map[string]any{"image": map[string]any{"tag": "2.6.0"}})
	if img := at(t, onlyDoc(t, out, "deployment.yaml"), "spec", "template", "spec",
		"containers", 0, "image"); img != "ghcr.io/jasonmassie01/pg_sage:2.6.0" {
		t.Fatalf("pinned image = %v", img)
	}
}

// CHECK-02: the Role grants only get, only on the named Clusters.
func TestHelmChartCNPGRoleIsGetOnly(t *testing.T) {
	out := mustRender(t, map[string]any{"cnpg": map[string]any{"cluster": "pg-main",
		"rbac": map[string]any{"clusters": []any{"pg-other", "pg-main"}}}})
	docs := renderedDocs(t, "role.yaml", out["role.yaml"])
	if len(docs) != 2 || docs[0]["kind"] != "Role" || docs[1]["kind"] != "RoleBinding" {
		t.Fatalf("role.yaml = %v", docs)
	}
	rules := at(t, docs[0], "rules").([]any)
	want := map[string]any{"apiGroups": []any{"postgresql.cnpg.io"},
		"resources": []any{"clusters"}, "verbs": []any{"get"},
		"resourceNames": []any{"pg-main", "pg-other"}}
	if len(rules) != 1 || !reflect.DeepEqual(rules[0], want) {
		t.Fatalf("rules = %v, want %v", rules, want)
	}
	if at(t, docs[0], "metadata", "namespace") != "db" ||
		at(t, docs[1], "subjects", 0, "name") != "demo-pg-sage" {
		t.Fatalf("Role %v binding %v", docs[0]["metadata"], docs[1]["subjects"])
	}
	pod := at(t, onlyDoc(t, out, "deployment.yaml"), "spec", "template", "spec")
	if at(t, pod, "automountServiceAccountToken") != true {
		t.Fatal("the CNPG adapter needs the service-account token")
	}
	off := mustRender(t, map[string]any{"cnpg": map[string]any{"cluster": "pg-main",
		"rbac": map[string]any{"create": false}}})
	if len(renderedDocs(t, "role.yaml", off["role.yaml"])) != 0 {
		t.Fatal("rbac.create false renders no Role")
	}
}

// The ConfigMap's config.yaml is a valid pg_sage config; cnpg.cluster
// reaches postgres.cnpg with the release namespace.
func TestHelmChartConfigLoads(t *testing.T) {
	out := mustRender(t, map[string]any{"cnpg": map[string]any{"cluster": "pg-main"},
		"config": map[string]any{"postgres": map[string]any{"sslmode": "require"}}})
	cm := onlyDoc(t, out, "configmap.yaml")
	text, ok := at(t, cm, "data", "config.yaml").(string)
	if !ok || text == "" {
		t.Fatalf("configmap = %v", cm)
	}
	t.Setenv("SAGE_DATABASE_URL", "postgres://sage@pg-main-rw.db:5432/app")
	cfg, err := loadYAMLResult(t, text)
	if err != nil {
		t.Fatalf("the chart config.yaml does not load: %v\n%s", err, text)
	}
	if cfg.Mode != "standalone" || cfg.Postgres.SSLMode != "require" ||
		cfg.Postgres.CNPG != (CNPGConfig{Cluster: "pg-main", Namespace: "db"}) ||
		cfg.API.ListenAddr != "0.0.0.0:8080" {
		t.Fatalf("config = mode %q sslmode %q cnpg %+v api %q", cfg.Mode,
			cfg.Postgres.SSLMode, cfg.Postgres.CNPG, cfg.API.ListenAddr)
	}
}

func TestHelmChartOptionalObjects(t *testing.T) {
	out := mustRender(t, map[string]any{
		"serviceMonitor": map[string]any{"enabled": true},
		"networkPolicy":  map[string]any{"enabled": true},
		"tls":            map[string]any{"existingSecret": "pg-sage-tls"},
		"secrets":        map[string]any{"llmAPIKeyKey": "llm-api-key"}})
	if k := onlyDoc(t, out, "servicemonitor.yaml")["kind"]; k != "ServiceMonitor" {
		t.Fatalf("servicemonitor kind %v", k)
	}
	if k := onlyDoc(t, out, "networkpolicy.yaml")["kind"]; k != "NetworkPolicy" {
		t.Fatalf("networkpolicy kind %v", k)
	}
	c := at(t, onlyDoc(t, out, "deployment.yaml"), "spec", "template", "spec",
		"containers", 0)
	env := map[string]any{}
	for _, e := range at(t, c, "env").([]any) {
		env[at(t, e, "name").(string)] = at(t, e, "value")
	}
	if env["SAGE_TLS_CERT"] != "/var/run/tls/tls.crt" ||
		env["SAGE_LLM_API_KEY_FILE"] != "/var/run/secrets/pg-sage/llm-api-key" ||
		at(t, c, "livenessProbe", "httpGet", "scheme") != "HTTPS" {
		t.Fatalf("env %v probe %v", env, at(t, c, "livenessProbe"))
	}
}

// data_dir is writable under the read-only root filesystem: an emptyDir by
// default, the existing claim when one is named.
func TestHelmChartDataVolume(t *testing.T) {
	volume := func(out map[string]string) map[string]any {
		pod := at(t, onlyDoc(t, out, "deployment.yaml"), "spec", "template", "spec")
		for _, v := range at(t, pod, "volumes").([]any) {
			if at(t, v, "name") == "data" {
				return v.(map[string]any)
			}
		}
		t.Fatal("no data volume")
		return nil
	}
	if v := volume(mustRender(t, nil)); v["emptyDir"] == nil {
		t.Fatalf("default data volume = %v", v)
	}
	out := mustRender(t, map[string]any{
		"persistence": map[string]any{"existingClaim": "pg-sage-data"},
		"secrets":     map[string]any{"encryptionKeyKey": "encryption-key"}})
	if v := volume(out); at(t, v, "persistentVolumeClaim", "claimName") != "pg-sage-data" {
		t.Fatalf("claimed data volume = %v", v)
	}
	if !strings.Contains(out["deployment.yaml"], "/var/run/secrets/pg-sage/encryption-key") {
		t.Fatal("encryptionKeyKey does not reach SAGE_ENCRYPTION_KEY_FILE")
	}
}

// The chart tracks the release: appVersion is the VERSION file.
func TestHelmChartAppVersionIsTheRelease(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	meta := readYAMLMap(t, filepath.Join(chartDir, "Chart.yaml"))
	if v := strings.TrimSpace(string(raw)); meta["appVersion"] != v || meta["version"] != v {
		t.Fatalf("Chart.yaml version %v appVersion %v, VERSION %s", meta["version"],
			meta["appVersion"], v)
	}
}

// The chart README documents every top-level value.
func TestHelmChartReadmeNamesEveryValue(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "README.md"))
	if err != nil {
		t.Fatalf("chart README: %v", err)
	}
	for key := range readYAMLMap(t, filepath.Join(chartDir, "values.yaml")) {
		if !strings.Contains(string(raw), "`"+key) {
			t.Errorf("README.md does not document %s", key)
		}
	}
}
