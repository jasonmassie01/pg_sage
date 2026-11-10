package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// docs/fleet.md must match the product: every config key, metric and API
// path it names exists, every key of its sample JSON is one the API
// serves, its relative links resolve, and readers can reach it from the
// site nav and the fleet walkthrough.

// No concurrent access tests: these checks read files and never share state.

const (
	fleetDocPath        = "../../../docs/fleet.md"
	fleetWalkthrough    = "../../../docs/walkthrough-fleet.md"
	mkdocsConfigPath    = "../../../mkdocs.yml"
	fleetDocMinJSONKeys = 20
)

var (
	jsonFence    = regexp.MustCompile("(?s)```json\\r?\\n(.*?)```")
	markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	// Dotted tokens of fleet.md that are API response fields or SQL
	// objects, not config keys.
	notFleetConfigKey = regexp.MustCompile(`^(status|summary|public)\.`)
)

func readFleetDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(fleetDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", fleetDocPath, err)
	}
	return string(raw)
}

func TestFleetDocConfigKeysExist(t *testing.T) {
	doc := readFleetDoc(t)
	checked := 0
	for _, m := range backtickToken.FindAllStringSubmatch(doc, -1) {
		token := strings.TrimSuffix(m[1], ":")
		if !dottedKey.MatchString(token) || notConfigKey.MatchString(token) ||
			notFleetConfigKey.MatchString(token) {
			continue
		}
		checked++
		if !yamlPathExists(reflect.TypeOf(Config{}), strings.Split(token, ".")) {
			t.Errorf("docs/fleet.md names config key %q, which does not exist", token)
		}
	}
	// Every fleet_learning key is documented, so at least those ten.
	if checked < 10 {
		t.Fatalf("docs/fleet.md names %d config keys, want every fleet_learning key",
			checked)
	}
	missing := undocumentedFleetLearningKeys(doc)
	if len(missing) > 0 {
		t.Errorf("docs/fleet.md does not document %v", missing)
	}
}

// undocumentedFleetLearningKeys are the fleet_learning keys doc never names.
func undocumentedFleetLearningKeys(doc string) []string {
	var out []string
	typ := reflect.TypeOf(FleetLearningConfig{})
	for i := 0; i < typ.NumField(); i++ {
		key := "fleet_learning." + strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
		if !strings.Contains(doc, "`"+key+"`") {
			out = append(out, key)
		}
	}
	return out
}

func TestFleetDocMetricsAndPathsExist(t *testing.T) {
	doc := readFleetDoc(t)
	src := sourceText(t, "../../cmd/pg_sage_sidecar", "../../internal/api",
		"../../internal/fleet")
	for _, want := range []string{"/api/v1/fleet/findings", "/api/v1/fleet/lookalikes",
		"/api/v1/fleet/leader", "pg_sage_leader", "pg_sage_fleet_healthy"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/fleet.md does not mention %s", want)
		}
	}
	for _, re := range []*regexp.Regexp{metricName, apiPath} {
		for _, token := range re.FindAllString(doc, -1) {
			if !strings.Contains(src, token) {
				t.Errorf("docs/fleet.md names %s, which the code does not define", token)
			}
		}
	}
}

func TestFleetDocSampleJSONKeysAreServed(t *testing.T) {
	doc := readFleetDoc(t)
	src := sourceText(t, "../../internal/api", "../../internal/fleetlearn",
		"../../internal/leader", "../../internal/fleet")
	blocks := jsonFence.FindAllStringSubmatch(doc, -1)
	if len(blocks) < 3 {
		t.Fatalf("docs/fleet.md has %d JSON samples, want one per fleet route", len(blocks))
	}
	keys := map[string]bool{}
	for i, b := range blocks {
		var v any
		if err := json.Unmarshal([]byte(b[1]), &v); err != nil {
			t.Fatalf("docs/fleet.md JSON sample %d does not parse: %v", i+1, err)
		}
		collectJSONKeys(v, keys)
	}
	if len(keys) < fleetDocMinJSONKeys {
		t.Fatalf("the samples hold %d distinct keys, want at least %d", len(keys),
			fleetDocMinJSONKeys)
	}
	for _, k := range sortedKeys(keys) {
		if !servedJSONKey(src, k) {
			t.Errorf("docs/fleet.md sample key %q is not a JSON field the API serves", k)
		}
	}
}

// servedJSONKey reports whether src defines key as a struct JSON tag or a
// map literal key.
func servedJSONKey(src, key string) bool {
	return strings.Contains(src, `json:"`+key+`"`) ||
		strings.Contains(src, `json:"`+key+`,`) || strings.Contains(src, `"`+key+`": `)
}

func collectJSONKeys(v any, into map[string]bool) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			into[k] = true
			collectJSONKeys(child, into)
		}
	case []any:
		for _, child := range x {
			collectJSONKeys(child, into)
		}
	}
}

func TestFleetDocLinksResolve(t *testing.T) {
	for _, path := range []string{fleetDocPath, fleetWalkthrough} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		links := markdownLink.FindAllStringSubmatch(string(raw), -1)
		if len(links) == 0 {
			t.Fatalf("%s links to no other page", path)
		}
		for _, m := range links {
			if msg := brokenDocLink(path, m[1]); msg != "" {
				t.Errorf("%s: %s", path, msg)
			}
		}
	}
}

// brokenDocLink explains why the relative link target in the doc at
// from does not resolve to a file and heading ("#x" is a heading of from);
// "" when it does or is not relative.
func brokenDocLink(from, target string) string {
	if strings.Contains(target, "://") {
		return ""
	}
	file, anchor, _ := strings.Cut(target, "#")
	path := from
	if file != "" {
		path = filepath.Join(filepath.Dir(from), file)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "links to " + target + ", which does not exist"
	}
	if anchor != "" && !hasHeadingAnchor(string(raw), anchor) {
		return "links to " + target + ", but " + path + " has no such heading"
	}
	return ""
}

// hasHeadingAnchor reports whether doc has a heading whose mkdocs slug is
// anchor (lower case, punctuation dropped, spaces as hyphens).
func hasHeadingAnchor(doc, anchor string) bool {
	drop := regexp.MustCompile(`[^a-z0-9 _-]`)
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		title := strings.TrimSpace(strings.TrimLeft(line, "#"))
		slug := drop.ReplaceAllString(strings.ToLower(title), "")
		if strings.ReplaceAll(strings.TrimSpace(slug), " ", "-") == anchor {
			return true
		}
	}
	return false
}

func TestHasHeadingAnchor(t *testing.T) {
	doc := "# Security\n\n## Fleet-wide stop\n\n### Who may `stop`?\n"
	for anchor, want := range map[string]bool{"fleet-wide-stop": true, "security": true,
		"who-may-stop": true, "fleet-stop": false, "": false} {
		if got := hasHeadingAnchor(doc, anchor); got != want {
			t.Errorf("hasHeadingAnchor(%q) = %v, want %v", anchor, got, want)
		}
	}
}

func TestFleetDocIsReachable(t *testing.T) {
	nav, err := os.ReadFile(mkdocsConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", mkdocsConfigPath, err)
	}
	if !regexp.MustCompile(`(?m)^\s+- [^\n]*: fleet\.md\s*$`).Match(nav) {
		t.Error("mkdocs.yml nav does not list fleet.md")
	}
	walk, err := os.ReadFile(fleetWalkthrough)
	if err != nil {
		t.Fatalf("read %s: %v", fleetWalkthrough, err)
	}
	if !strings.Contains(string(walk), "](fleet.md") {
		t.Error("docs/walkthrough-fleet.md does not link to fleet.md")
	}
	if strings.Contains(string(walk), "Settings | Emergency") {
		t.Error("docs/walkthrough-fleet.md still sends readers to Settings for " +
			"emergency controls; they are in the header")
	}
	if regexp.MustCompile(`(?m)^meta_db: .*/app_production`).Match(walk) {
		t.Error("docs/walkthrough-fleet.md uses the monitored app_production as " +
			"meta_db; the example uses a dedicated control database")
	}
}
