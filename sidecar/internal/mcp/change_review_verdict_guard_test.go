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

var verdictArguments = map[string]bool{"verdict": true, "dba_verdict": true,
	"direction": true, "dba_direction": true, "dba_reason": true}

func TestNoMCPToolTakesADBAVerdict(t *testing.T) {
	tools := NewServer(&crRecordingBackend{}).Tools()
	if len(tools) < 4 {
		t.Fatalf("%d tools registered; the check proves nothing", len(tools))
	}
	for _, tool := range tools {
		schema := decodeSchema(t, tool.InputSchema)
		props, _ := schema["properties"].(map[string]any)
		for name := range props {
			if verdictArguments[strings.ToLower(name)] {
				t.Errorf("tool %s takes %q: no MCP tool may record a DBA verdict",
					tool.Name, name)
			}
		}
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
