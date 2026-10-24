package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// No MCP tool records a DBA verdict, whatever it is called: DBA verdicts
// come only from operator and admin sessions (REST and the UI). Every
// registered tool's schema is checked for verdict arguments, the backend
// the tools reach has no decision method, and no non-test source of this
// package calls a Decide method. No concurrency test: the checks read
// static definitions.

// dbaArguments are a DBA verdict's fields under any tool's name;
// reviewArguments are what a verdict looks like on a tool about change
// reviews (other tools, an SRE investigation's review say, have verdicts
// of their own).
var (
	dbaArguments = map[string]bool{"dba_verdict": true, "dba_direction": true,
		"dba_reason": true, "dba_actor": true}
	reviewArguments = map[string]bool{"verdict": true, "direction": true, "reason": true,
		"concur": true, "override": true}
)

// aboutChangeReviews reports whether a tool reaches change reviews: by
// name or by what it says it does.
func aboutChangeReviews(name, description string) bool {
	text := strings.ToLower(name + " " + description)
	if changeReviewToolNames[name] {
		return true
	}
	for _, s := range []string{"change review", "change_review", "review_change",
		"change-review", "reviewed change"} {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

func TestNoMCPToolTakesADBAVerdict(t *testing.T) {
	tools := NewServer(&crRecordingBackend{}).Tools()
	reviewTools := 0
	for _, tool := range tools {
		schema := decodeSchema(t, tool.InputSchema)
		props, _ := schema["properties"].(map[string]any)
		about := aboutChangeReviews(tool.Name, tool.Description)
		if about {
			reviewTools++
		}
		for name := range props {
			n := strings.ToLower(name)
			if dbaArguments[n] || (about && reviewArguments[n]) {
				t.Errorf("tool %s takes %q: no MCP tool may record a DBA verdict",
					tool.Name, name)
			}
		}
	}
	if reviewTools < 4 {
		t.Fatalf("%d change-review tools found; the check proves nothing", reviewTools)
	}
}

func TestChangeReviewBackendHasNoDecision(t *testing.T) {
	backend := reflect.TypeOf((*ChangeReviewBackend)(nil)).Elem()
	if backend.NumMethod() == 0 {
		t.Fatal("the backend has no methods; the check proves nothing")
	}
	for i := 0; i < backend.NumMethod(); i++ {
		name := strings.ToLower(backend.Method(i).Name)
		for _, banned := range []string{"decide", "verdict", "concur", "override"} {
			if strings.Contains(name, banned) {
				t.Errorf("ChangeReviewBackend.%s reaches a DBA decision",
					backend.Method(i).Name)
			}
		}
	}
}

var decideCall = regexp.MustCompile(`\.Decide\(`)

func TestNoMCPSourceCallsDecide(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list sources: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if decideCall.Match(raw) {
			t.Errorf("%s calls a Decide method: no MCP path may record a DBA verdict", f)
		}
	}
}
