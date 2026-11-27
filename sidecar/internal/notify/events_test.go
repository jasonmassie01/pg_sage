package notify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestActionExecutedEvent(t *testing.T) {
	evt := ActionExecutedEvent("Create index", "CREATE INDEX", "mydb")
	if evt.Type != "action_executed" {
		t.Errorf("Type = %q, want action_executed", evt.Type)
	}
	if evt.Severity != "info" {
		t.Errorf("Severity = %q, want info", evt.Severity)
	}
	if evt.Data["database"] != "mydb" {
		t.Errorf("Data[database] = %v, want mydb", evt.Data["database"])
	}
}

func TestActionFailedEvent(t *testing.T) {
	evt := ActionFailedEvent("Drop index", "DROP INDEX", "mydb", "timeout")
	if evt.Type != "action_failed" {
		t.Errorf("Type = %q, want action_failed", evt.Type)
	}
	if evt.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", evt.Severity)
	}
	if evt.Data["error"] != "timeout" {
		t.Errorf("Data[error] = %v, want timeout", evt.Data["error"])
	}
}

func TestApprovalNeededEvent(t *testing.T) {
	evt := ApprovalNeededEvent("Reindex", "REINDEX", "mydb", "high")
	if evt.Type != "approval_needed" {
		t.Errorf("Type = %q, want approval_needed", evt.Type)
	}
	if evt.Data["risk"] != "high" {
		t.Errorf("Data[risk] = %v, want high", evt.Data["risk"])
	}
}

func TestFindingCriticalEvent(t *testing.T) {
	evt := FindingCriticalEvent("Disk full", "90% used", "prod", "disk", "pg_data")
	if evt.Type != "finding_critical" {
		t.Errorf("Type = %q, want finding_critical", evt.Type)
	}
	if evt.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", evt.Severity)
	}
}

// constructorEvents builds one event from every exported constructor that
// returns an Event, with the lowest-severity input each one accepts.
// Incident events carry their incident's severity; their default rule
// floor is warning by design (G7-B06), so they are built at warning.
func constructorEvents() map[string]Event {
	fact := FactProposedEvent(FactCardRef{Database: "orders", FactID: 3,
		Title: "t"}, "b")
	queued := QueuedApprovalEvent("t", "ANALYZE t", "orders", "safe", 7)
	incident := IncidentInfo{ID: "i1", Database: "orders", Severity: "warning"}
	return map[string]Event{
		"ActionExecutedEvent":  ActionExecutedEvent("t", "ANALYZE t", "orders"),
		"ActionFailedEvent":    ActionFailedEvent("t", "ANALYZE t", "orders", "e"),
		"ApprovalNeededEvent":  ApprovalNeededEvent("t", "ANALYZE t", "orders", "safe"),
		"FindingCriticalEvent": FindingCriticalEvent("t", "d", "orders", "c", "o"),
		"FindingOpenedEvent":   FindingOpenedEvent("t", "d", "orders", "c", "o", "warning"),
		"FindingResolvedEvent": FindingResolvedEvent("t", "orders", "c", "o", "critical"),
		"QueryRewriteEvent":    QueryRewriteEvent("t", "q", "r", "why", "orders"),
		"ResolvedEvent":        ResolvedEvent(fact),
		"QueuedApprovalEvent":  queued,
		"WithApprovalCard":     WithApprovalCard(queued, CardRef{QueueID: 7}, "b"),
		"ApprovalOutcomeEvent": ApprovalOutcomeEvent(ApprovalOutcome{
			Database: "orders", QueueID: 7, Verdict: "verified"}),
		"FactProposedEvent": fact,
		"PlanProposedEvent": PlanProposedEvent(PlanCardRef{Database: "orders", PlanID: 4,
			Title: "t"}, "b"),
		"FactExpiredEvent":       FactExpiredEvent(FactExpiry{Database: "orders", FactID: 3}),
		"IncidentDetectedEvent":  IncidentDetectedEvent(incident),
		"IncidentEscalatedEvent": IncidentEscalatedEvent(incident),
		"IncidentResolvedEvent":  IncidentResolvedEvent(incident),
		"CustodianEscalationEvent": CustodianEscalationEvent(CustodianEscalation{
			Database: "orders", Target: "public.t", SQLSHA256: "ab", Failures: 3}),
		"CanaryInterruptedEvent": CanaryInterruptedEvent(CanaryInterrupted{
			RunID: "canary-1", Source: "orders"}),
		"InvestigationConcludedEvent": InvestigationConcludedEvent(ConclusionInfo{
			IncidentID: "i1", Database: "orders", State: "inconclusive",
			Severity: "warning"}),
	}
}

// alerting-03: an event below its type's default min_severity is silently
// dropped by every rule created with defaults.
func TestConstructorSeverityMeetsDefaultMinSeverity(t *testing.T) {
	for name, evt := range constructorEvents() {
		if _, ok := ValidSeverities[evt.Severity]; !ok {
			t.Errorf("%s: severity %q is not a valid severity", name, evt.Severity)
			continue
		}
		if min := DefaultMinSeverity(evt.Type); !SeverityMeetsMin(evt.Severity, min) {
			t.Errorf("%s: %s severity %q below default min_severity %q",
				name, evt.Type, evt.Severity, min)
		}
	}
}

// The table above must name every exported Event constructor, so a new
// one cannot drift from the default rules unnoticed.
func TestConstructorSeverityTableCoversEveryConstructor(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	fset := token.NewFileSet()
	var found []string
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && returnsEvent(fn) {
				found = append(found, fn.Name.Name)
			}
		}
	}
	sort.Strings(found)
	table := constructorEvents()
	if len(found) != len(table) {
		t.Errorf("constructors %v, table has %d entries", found, len(table))
	}
	for _, name := range found {
		if _, ok := table[name]; !ok {
			t.Errorf("constructor %s missing from constructorEvents", name)
		}
	}
}

func returnsEvent(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil ||
		len(fn.Type.Results.List) != 1 {
		return false
	}
	id, ok := fn.Type.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "Event"
}

// Invalid input: fact cards stay approval_needed events; no separate type
// is accepted for rules.
func TestFactCardTypeIsApprovalNeeded(t *testing.T) {
	if evt := FactProposedEvent(FactCardRef{FactID: 1}, "b"); evt.Type != "approval_needed" {
		t.Fatalf("fact card type = %q, want approval_needed", evt.Type)
	}
	for _, bad := range []string{"fact_proposed", "", "APPROVAL_NEEDED", "approval_outcome"} {
		if ValidEventTypes[bad] {
			t.Errorf("ValidEventTypes accepts %q", bad)
		}
	}
}
