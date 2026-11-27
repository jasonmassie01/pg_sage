package lakebaseid

import (
	"strings"
	"testing"
)

const (
	envOne  = "SAGE_LAKEBASE_ENDPOINT"
	envMany = "SAGE_LAKEBASE_ENDPOINTS"
	primary = "projects/search/branches/test/endpoints/primary"
	replica = "projects/1abc/branches/test/endpoints/replica"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestEndpointForHostResolvesTheEnvironment(t *testing.T) {
	many := `{"ep-a.example.com": "` + primary + `", "EP-B.example.com.": "` + replica + `"}`
	for _, tc := range []struct {
		name, host, want string
		vars             map[string]string
	}{
		{"no mapping is ordinary PostgreSQL", "ep-a.example.com", "", nil},
		{"single endpoint applies to any host", "anything.example.com", primary,
			map[string]string{envOne: primary}},
		{"fleet map picks the host", "ep-a.example.com", primary,
			map[string]string{envMany: many}},
		{"fleet map ignores case and trailing dot", "ep-b.EXAMPLE.com", replica,
			map[string]string{envMany: many}},
		{"fleet map leaves an unmapped host alone", "ep-c.example.com", "",
			map[string]string{envMany: many}},
		{"empty fleet map", "ep-a.example.com", "", map[string]string{envMany: "{}"}},
		{"empty host matches nothing", "", "", map[string]string{envMany: many}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EndpointForHost(env(tc.vars), tc.host)
			if err != nil {
				t.Fatalf("EndpointForHost: %v", err)
			}
			if got != tc.want {
				t.Fatalf("EndpointForHost(%q) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

func TestEndpointForHostRefusesBadEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		vars       map[string]string
	}{
		{"both variables set", "set only one of SAGE_LAKEBASE_ENDPOINT and SAGE_LAKEBASE_ENDPOINTS",
			map[string]string{envOne: primary, envMany: `{"h.example.com": "` + primary + `"}`}},
		{"fleet map is not JSON", "must be a JSON object",
			map[string]string{envMany: "h.example.com=" + primary}},
		{"fleet map is a list", "must be a JSON object",
			map[string]string{envMany: `["` + primary + `"]`}},
		{"fleet map holds a bad resource for another host",
			"invalid Lakebase endpoint resource path in SAGE_LAKEBASE_ENDPOINTS",
			map[string]string{
				envMany: `{"other.example.com": "projects/-bad/branches/b/endpoints/c"}`}},
		{"single endpoint is malformed", "projects/<id>/branches/<id>/endpoints/<id>",
			map[string]string{envOne: "projects/search"}},
		{"single endpoint segment too long", "projects/<id>/branches/<id>/endpoints/<id>",
			map[string]string{
				envOne: "projects/a/branches/b/endpoints/" + strings.Repeat("e", 64)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EndpointForHost(env(tc.vars), "h.example.com")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("EndpointForHost error = %v, want one containing %q", err, tc.want)
			}
			if got != "" {
				t.Fatalf("refused environment still returned %q", got)
			}
		})
	}
}

func TestEndpointForHostAcceptsEveryTableVerdict(t *testing.T) {
	for _, c := range loadCases(t) {
		if c.Resource == "" {
			continue // an empty variable means "not set", not an invalid value
		}
		got, err := EndpointForHost(env(map[string]string{envOne: c.Resource}), "h.example.com")
		if (err == nil) != c.Valid {
			t.Errorf("%s: SAGE_LAKEBASE_ENDPOINT=%q error = %v, want valid=%v",
				c.Name, c.Resource, err, c.Valid)
		}
		if c.Valid && got != c.Resource {
			t.Errorf("%s: got %q, want %q", c.Name, got, c.Resource)
		}
	}
}

func TestEndpointForHostNilGetenv(t *testing.T) {
	got, err := EndpointForHost(nil, "h.example.com")
	if err == nil || got != "" {
		t.Fatalf("nil getenv = %q, %v; want a refusal", got, err)
	}
}
