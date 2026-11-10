package lakebaseid

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// resourceCase is one row of testdata/resources.json, the verdict table the
// Go validators and deploy/databricks-apps/configure.py must all agree on.
type resourceCase struct {
	Name     string `json:"name"`
	Resource string `json:"resource"`
	Valid    bool   `json:"valid"`
}

// loadCases reads the shared verdict table and fails loudly when it is
// missing, so an empty table can never pass.
func loadCases(t *testing.T) []resourceCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/resources.json")
	if err != nil {
		t.Fatalf("read verdict table: %v", err)
	}
	var cases []resourceCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse verdict table: %v", err)
	}
	return cases
}

func TestPatternIsTheLakebaseSegmentRule(t *testing.T) {
	if Pattern != `[a-z0-9][a-z0-9-]{0,62}` {
		t.Fatalf("Pattern = %q; configure.py and the docs copy this text", Pattern)
	}
}

func TestVerdictTableCoversRequiredCases(t *testing.T) {
	names := map[string]bool{}
	valid, invalid := 0, 0
	for _, c := range loadCases(t) {
		names[c.Name] = true
		if c.Valid {
			valid++
		} else {
			invalid++
		}
	}
	for _, want := range []string{"leading digit project", "leading hyphen project",
		"63 character endpoint", "64 character endpoint", "uppercase project",
		"extra segment", "missing endpoint segment", "empty"} {
		if !names[want] {
			t.Errorf("verdict table lacks case %q", want)
		}
	}
	if valid < 5 || invalid < 10 {
		t.Fatalf("verdict table too thin: %d valid, %d invalid", valid, invalid)
	}
}

func TestValidAndParseAgreeWithTheTable(t *testing.T) {
	for _, c := range loadCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			if got := Valid(c.Resource); got != c.Valid {
				t.Fatalf("Valid(%q) = %v, want %v", c.Resource, got, c.Valid)
			}
			project, branch, endpoint, err := Parse(c.Resource)
			if (err == nil) != c.Valid {
				t.Fatalf("Parse(%q) error = %v, want valid=%v", c.Resource, err, c.Valid)
			}
			if !c.Valid {
				if project != "" || branch != "" || endpoint != "" {
					t.Fatalf("rejected resource returned parts %q %q %q",
						project, branch, endpoint)
				}
				if !strings.Contains(err.Error(), "projects/<id>/branches/<id>/endpoints/<id>") {
					t.Fatalf("error %q does not name the expected form", err)
				}
				return
			}
			rebuilt := "projects/" + project + "/branches/" + branch + "/endpoints/" + endpoint
			if rebuilt != c.Resource {
				t.Fatalf("Parse parts rebuild %q, want %q", rebuilt, c.Resource)
			}
		})
	}
}

func TestParseSplitsEachSegment(t *testing.T) {
	project, branch, endpoint, err := Parse("projects/1abc/branches/br-2/endpoints/ep-3")
	if err != nil {
		t.Fatal(err)
	}
	if project != "1abc" || branch != "br-2" || endpoint != "ep-3" {
		t.Fatalf("Parse = %q %q %q", project, branch, endpoint)
	}
}

// No concurrent access tests: Valid, Parse and EndpointForHost hold no state;
// the compiled pattern is read-only and safe for concurrent use.
