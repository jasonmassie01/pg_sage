package executor

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/backendguard"
	"github.com/pg-sage/sidecar/internal/cases"
)

var excludedAppPattern = regexp.MustCompile(`application_name NOT ILIKE '([^']+)'`)

func excludedApps(sql string) []string {
	var out []string
	for _, m := range excludedAppPattern.FindAllStringSubmatch(sql, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// casesSignalScripts renders the case playbook's guarded cancel and
// terminate scripts for a critical lock incident with one active blocker.
func casesSignalScripts(t *testing.T) map[string]string {
	t.Helper()
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	blocker := &cases.IncidentBlocker{PID: 4242, BackendStart: at, QueryStart: at,
		QuerySHA: "deadbeefcafef00d", State: "active"}
	c := cases.ProjectIncident(cases.SourceIncident{
		ID: "inc", DatabaseName: "prod", Severity: cases.SeverityCritical,
		RootCause: "Lock chain: root blocker pid 4242 (active) blocks 1 sessions",
		SignalIDs: []string{"lock_contention"},
		CausalChain: []cases.IncidentChainLink{{Order: 2, Signal: "lock_contention",
			Description: "Root blocker", Blocker: blocker}},
	})
	out := map[string]string{}
	for _, candidate := range c.ActionCandidates {
		switch candidate.ActionType {
		case "cancel_backend":
			out["cancel"] = candidate.ProposedSQL
		case "terminate_backend":
			out["terminate"] = candidate.ProposedSQL
		}
	}
	if out["cancel"] == "" || out["terminate"] == "" {
		t.Fatalf("case playbook rendered no guarded scripts: %+v", c.ActionCandidates)
	}
	return out
}

// Parity (playbooks-01): the case scripts and the executor's approved
// signal SQL exclude exactly the same sessions, through backendguard.
func TestBackendGuardParityWithCaseScripts(t *testing.T) {
	scripts := casesSignalScripts(t)
	executorSQL := map[string]string{
		"cancel": cancelMatchingBackendSQL, "terminate": terminateMatchingBackendSQL,
	}
	for op, sql := range executorSQL {
		if !strings.Contains(sql, backendguard.ProtectedPredicate) {
			t.Fatalf("executor %s SQL does not use backendguard.ProtectedPredicate", op)
		}
		got, want := excludedApps(scripts[op]), excludedApps(sql)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s exclusions: case script %v, executor %v", op, got, want)
		}
	}
	if !strings.Contains(terminateMatchingBackendSQL, backendguard.TerminatePredicate) ||
		!strings.Contains(terminateMatchingBackendSQL, backendguard.TerminateJoin) {
		t.Fatal("executor terminate SQL does not use backendguard terminate guards")
	}
	if len(excludedApps(cancelMatchingBackendSQL)) != 4 {
		t.Fatalf("executor cancel exclusions = %v, want 4", excludedApps(cancelMatchingBackendSQL))
	}
}
