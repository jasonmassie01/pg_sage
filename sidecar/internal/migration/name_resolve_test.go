package migration

import (
	"reflect"
	"testing"
)

// migration-05: an unqualified table resolves through the DDL role's
// search_path, then public, then a unique relname; several matches are
// ambiguous.

func TestParseSearchPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config []string
		want   []string
	}{
		{"role setting", []string{"search_path=app, public"}, []string{"app", "public"}},
		{"quoted and $user", []string{`search_path="$user", "My Schema", app`},
			[]string{"$user", "My Schema", "app"}},
		{"other settings ignored", []string{"work_mem=64MB", "search_path=app"},
			[]string{"app"}},
		{"no search_path", []string{"work_mem=64MB"}, nil},
		{"empty", nil, nil},
		{"blank value", []string{"search_path="}, nil},
	} {
		if got := parseSearchPath(tc.config); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: parseSearchPath(%q) = %q, want %q", tc.name, tc.config, got, tc.want)
		}
	}
}

func TestResolveSchema(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []string
		path       []string
		role       string
		want       string
		ambiguous  bool
	}{
		{"search_path wins over public", []string{"public", "app"}, []string{"app"}, "",
			"app", false},
		{"search_path order", []string{"b", "a"}, []string{"a", "b"}, "", "a", false},
		{"$user is the role", []string{"alice", "public"}, []string{"$user", "public"},
			"alice", "alice", false},
		{"$user without role skipped", []string{"public"}, []string{"$user"}, "",
			"public", false},
		{"pg_temp never chosen", []string{"pg_temp", "public"}, []string{"pg_temp"}, "",
			"public", false},
		{"public fallback", []string{"public", "x"}, nil, "", "public", false},
		{"unique relname", []string{"only"}, []string{"app"}, "", "only", false},
		{"ambiguous", []string{"s1", "s2"}, []string{"app"}, "", "", true},
		{"no table", nil, []string{"app"}, "", "", false},
	} {
		got, amb := resolveSchema(tc.candidates, tc.path, tc.role)
		if got != tc.want || amb != tc.ambiguous {
			t.Fatalf("%s: resolveSchema = %q, %v; want %q, %v", tc.name, got, amb,
				tc.want, tc.ambiguous)
		}
	}
}
