# Autonomous DBA First Slice Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the first autonomous DBA slice: case projection, typed `analyze_table` action contract, unified action timeline shape, Shadow Mode proof, and UI navigation contraction without breaking existing findings/actions flows.

**Architecture:** Add a read-model Case layer over existing findings, incidents, query hints, forecasts, and schema lint before changing storage. Introduce typed action metadata around existing action/finding data, starting with `analyze_table`, then expose the new model through additive API endpoints and UI pages. Keep old endpoints/routes during the transition until the new destinations are verified.

**Tech Stack:** Go sidecar (`net/http`, `pgx`, existing `api`, `store`, `analyzer`, `executor`, `fleet` packages), embedded React SPA, Playwright e2e, Go unit/integration tests with `go test -cover -count=1 ./...`.

---

## Build Boundary

This plan implements only the first slice from:

- `C:/Users/jmass/pg_sage/specs/autonomous-dba-product-spec-2026-04-27.md`

Must ship in this slice:

- Case projection API over existing records.
- Cases UI route replacing Recommendations as the primary work queue.
- Overview copy/structure centered on agent state and next decisions.
- Actions timeline response shape that can combine pending and executed actions.
- `analyze_table` typed action contract metadata and validation.
- Shadow Mode report showing what pg_sage would have done under `auto_safe`.
- Navigation contraction behind a compatibility path.

Explicitly out of scope:

- Direct high-risk DDL execution.
- Full DDL operator.
- Full incident playbook library.
- Materialized view automation.
- Full provider matrix.
- Removing old storage tables.

---

## File Structure

### Backend Create

- `sidecar/internal/cases/case.go`
  - Case domain types, state constants, risk/policy labels, score helpers.
- `sidecar/internal/cases/projector.go`
  - Projection from findings, incidents, query hints, and forecasts into cases.
- `sidecar/internal/cases/identity.go`
  - Stable identity key generation and dedup helpers.
- `sidecar/internal/cases/shadow.go`
  - Shadow Mode summary over projected cases/action candidates.
- `sidecar/internal/cases/projector_test.go`
  - Projection, dedup, ephemeral, scoring tests.
- `sidecar/internal/cases/shadow_test.go`
  - Shadow Mode report tests.
- `sidecar/internal/api/cases_handlers.go`
  - `GET /api/v1/cases`, `GET /api/v1/cases/{id}`, `GET /api/v1/shadow-report`.
- `sidecar/internal/api/cases_handlers_test.go`
  - Handler validation and response shape tests.
- `sidecar/internal/executor/action_contract.go`
  - Typed action contract types and `analyze_table` contract.
- `sidecar/internal/executor/action_contract_test.go`
  - Contract validation tests.
- `sidecar/web/src/pages/CasesPage.jsx`
  - Unified case list/detail UI.
- `sidecar/web/src/pages/CasesPage.test.jsx`
  - Component behavior tests.
- `sidecar/web/src/pages/ShadowModePage.jsx`
  - Optional admin/readiness report page or Settings subview component.
- `sidecar/web/src/pages/ShadowModePage.test.jsx`
  - Shadow report UI tests.
- `e2e/cases.spec.ts`
  - Browser-level Cases workflow.
- `e2e/autonomous-first-slice.spec.ts`
  - Navigation, Shadow Mode, old-route compatibility smoke.

### Backend Modify

- `sidecar/internal/api/router.go`
  - Register additive case/shadow routes.
- `sidecar/internal/api/api_test.go`
  - Add route registration coverage.
- `sidecar/internal/api/handlers.go`
  - Reuse existing finding queries where appropriate.
- `sidecar/internal/api/handlers_new.go`
  - Reuse incident/query-hint/forecast query helpers where appropriate.
- `sidecar/internal/executor/analyze.go`
  - Wire contract metadata, post-check labels, and `expires_at` logic only if needed for the existing ANALYZE path.
- `sidecar/internal/store/action_store.go`
  - Add timeline/projection helpers only if existing action listing cannot provide the new response shape.
- `sidecar/internal/store/action_expiry.go`
  - Reuse or extend existing expiry logic for proposed actions.

### Frontend Modify

- `sidecar/web/src/App.jsx`
  - Route `#/cases` to `CasesPage`; keep `#/findings` as compatibility alias during transition.
- `sidecar/web/src/components/Layout.jsx`
  - Contract nav toward Overview, Cases, Actions, Fleet, Settings.
- `sidecar/web/src/pages/Dashboard.jsx`
  - Rename product semantics to Overview and show agent-state/decision sections.
- `sidecar/web/src/pages/Actions.jsx`
  - Add timeline-oriented defaults without removing current pending/executed flows until tests pass.
- `sidecar/web/src/pages/SettingsPage.jsx`
  - Add or link Shadow Mode readiness report under autonomy/policy area.
- `sidecar/web/src/components/CommandPalette.jsx`
  - Update route labels to Overview, Cases, Actions, Fleet, Settings.

### Docs And Tracking

- `tasks/todo.md`
  - Add checklist for this first slice.
- `docs/superpowers/plans/2026-04-27-autonomous-dba-first-slice.md`
  - This plan.

---

## Implementation Strategy

Use TDD for every behavioral change:

1. Write failing tests.
2. Run targeted tests and verify the expected failure.
3. Implement the smallest code that passes.
4. Run targeted tests.
5. Only after the slice is green, run broader verification.

Subagent split:

- Worker A: `sidecar/internal/cases` domain and projector.
- Worker B: API handlers and route tests.
- Worker C: typed action contract and shadow report.
- Worker D: React Cases/Overview/nav.
- Worker E: e2e verification and compatibility.

Workers are not alone in the codebase. Do not revert changes made by others; adapt to the merged shape.

---

## Task 1: Case Domain Types And Identity

**Files:**
- Create: `sidecar/internal/cases/case.go`
- Create: `sidecar/internal/cases/identity.go`
- Create: `sidecar/internal/cases/projector_test.go`

- [ ] **Step 1: Write failing tests for identity and required fields**

Create `sidecar/internal/cases/projector_test.go` with these tests:

```go
package cases

import "testing"

func TestIdentityKeyFindingUsesStableProblemFields(t *testing.T) {
	f := SourceFinding{
		DatabaseName:     "prod",
		Category:         "schema_lint:lint_no_primary_key",
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		RuleID:           "lint_no_primary_key",
	}

	got := IdentityKeyForFinding(f)
	want := "finding:prod:schema_lint:lint_no_primary_key:table:public.orders:lint_no_primary_key"
	if got != want {
		t.Fatalf("identity key = %q, want %q", got, want)
	}
}

func TestIdentityKeyQueryPrefersNormalizedFingerprint(t *testing.T) {
	f := SourceFinding{
		DatabaseName:     "prod",
		Category:         "query_tuning",
		ObjectType:       "query",
		ObjectIdentifier: "query_id:123",
		Detail: map[string]any{
			"normalized_query": "select * from orders where id = ?",
		},
	}

	got := IdentityKeyForFinding(f)
	want := "finding:prod:query_tuning:query:select * from orders where id = ?"
	if got != want {
		t.Fatalf("identity key = %q, want %q", got, want)
	}
}

func TestNewCaseRequiresWhyNowEvenWhenNotUrgent(t *testing.T) {
	c := NewCase(CaseInput{
		SourceType:   SourceFindingType,
		SourceID:     "42",
		DatabaseName: "prod",
		IdentityKey:  "finding:prod:test",
		Title:        "Test case",
		Severity:     SeverityInfo,
		Evidence: []Evidence{{
			Type:    "finding",
			Summary: "test evidence",
		}},
	})

	if c.WhyNow != "not urgent" {
		t.Fatalf("WhyNow = %q, want not urgent", c.WhyNow)
	}
	if c.State != StateOpen {
		t.Fatalf("State = %q, want %q", c.State, StateOpen)
	}
}
```

- [ ] **Step 2: Run the tests and verify RED**

Run:

```bash
go test -count=1 ./sidecar/internal/cases
```

Expected: FAIL because package/types do not exist.

- [ ] **Step 3: Implement case domain types**

Create `sidecar/internal/cases/case.go`:

```go
package cases

import "time"

const (
	SourceFindingType  = "finding"
	SourceIncidentType = "incident"
	SourceHintType     = "query_hint"
	SourceForecastType = "forecast"

	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"

	StateOpen              = "open"
	StateMonitoring        = "monitoring"
	StateWaitingApproval   = "waiting_for_approval"
	StateResolved          = "resolved"
	StateResolvedEphemeral = "resolved_ephemeral"
	StateBlocked           = "blocked"
)

type Evidence struct {
	Type    string         `json:"type"`
	Summary string         `json:"summary"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type ActionCandidate struct {
	ActionType      string     `json:"action_type"`
	RiskTier        string     `json:"risk_tier"`
	Confidence      float64    `json:"confidence"`
	ProposedSQL     string     `json:"proposed_sql,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	BlockedReason   string     `json:"blocked_reason,omitempty"`
	OutputModes      []string   `json:"output_modes,omitempty"`
	RollbackClass    string     `json:"rollback_class,omitempty"`
	VerificationPlan []string   `json:"verification_plan,omitempty"`
}

type Case struct {
	ID               string            `json:"case_id"`
	IdentityKey      string            `json:"identity_key"`
	SourceType       string            `json:"source_type"`
	SourceID         string            `json:"source_id"`
	DatabaseName     string            `json:"database_name"`
	Subsystem        string            `json:"subsystem"`
	Category         string            `json:"category"`
	Title            string            `json:"title"`
	Summary          string            `json:"summary"`
	Severity         string            `json:"severity"`
	ImpactScore      int               `json:"impact_score"`
	UrgencyScore     int               `json:"urgency_score"`
	State            string            `json:"state"`
	AffectedObjects  []string          `json:"affected_objects,omitempty"`
	AffectedQueries  []string          `json:"affected_queries,omitempty"`
	Evidence         []Evidence        `json:"evidence"`
	Hypothesis       string            `json:"hypothesis"`
	RootCause        string            `json:"root_cause,omitempty"`
	WhyNow           string            `json:"why_now"`
	ActionCandidates []ActionCandidate `json:"action_candidates"`
	BlockedReason    string            `json:"blocked_reason,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type CaseInput struct {
	SourceType       string
	SourceID         string
	DatabaseName     string
	IdentityKey      string
	Subsystem        string
	Category         string
	Title            string
	Summary          string
	Severity         string
	ImpactScore      int
	UrgencyScore     int
	State            string
	AffectedObjects  []string
	AffectedQueries  []string
	Evidence         []Evidence
	Hypothesis       string
	RootCause        string
	WhyNow           string
	ActionCandidates []ActionCandidate
	BlockedReason    string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func NewCase(in CaseInput) Case {
	now := time.Now().UTC()
	if in.State == "" {
		in.State = StateOpen
	}
	if in.WhyNow == "" {
		in.WhyNow = "not urgent"
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	if in.UpdatedAt.IsZero() {
		in.UpdatedAt = in.CreatedAt
	}
	return Case{
		ID:               in.IdentityKey,
		IdentityKey:      in.IdentityKey,
		SourceType:       in.SourceType,
		SourceID:         in.SourceID,
		DatabaseName:     in.DatabaseName,
		Subsystem:        in.Subsystem,
		Category:         in.Category,
		Title:            in.Title,
		Summary:          in.Summary,
		Severity:         in.Severity,
		ImpactScore:      in.ImpactScore,
		UrgencyScore:     in.UrgencyScore,
		State:            in.State,
		AffectedObjects:  in.AffectedObjects,
		AffectedQueries:  in.AffectedQueries,
		Evidence:         in.Evidence,
		Hypothesis:       in.Hypothesis,
		RootCause:        in.RootCause,
		WhyNow:           in.WhyNow,
		ActionCandidates: in.ActionCandidates,
		BlockedReason:    in.BlockedReason,
		CreatedAt:        in.CreatedAt,
		UpdatedAt:        in.UpdatedAt,
	}
}
```

- [ ] **Step 4: Implement identity helpers**

Create `sidecar/internal/cases/identity.go`:

```go
package cases

import (
	"fmt"
	"strings"
)

type SourceFinding struct {
	ID               string
	DatabaseName     string
	Category         string
	Severity         string
	ObjectType       string
	ObjectIdentifier string
	Title            string
	Recommendation   string
	RecommendedSQL   string
	RuleID           string
	Detail           map[string]any
}

func IdentityKeyForFinding(f SourceFinding) string {
	if f.ObjectType == "query" {
		if norm, ok := f.Detail["normalized_query"].(string); ok && norm != "" {
			return normalizeKey("finding", f.DatabaseName, f.Category, "query", norm)
		}
	}
	rule := f.RuleID
	if rule == "" {
		rule = f.Category
	}
	return normalizeKey(
		"finding",
		f.DatabaseName,
		f.Category,
		f.ObjectType,
		f.ObjectIdentifier,
		rule,
	)
}

func normalizeKey(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		p = strings.Join(strings.Fields(p), " ")
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	return fmt.Sprintf("%s", strings.Join(cleaned, ":"))
}
```

- [ ] **Step 5: Run targeted tests and verify GREEN**

Run:

```bash
go test -count=1 ./sidecar/internal/cases
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sidecar/internal/cases/case.go sidecar/internal/cases/identity.go sidecar/internal/cases/projector_test.go
git commit -m "feat(cases): add case identity model"
```

---

## Task 2: Case Projection From Existing Findings

**Files:**
- Create: `sidecar/internal/cases/projector.go`
- Modify: `sidecar/internal/cases/projector_test.go`

- [ ] **Step 1: Add failing projection tests**

Append to `sidecar/internal/cases/projector_test.go`:

```go
func TestProjectFindingCreatesActionableCase(t *testing.T) {
	f := SourceFinding{
		ID:               "42",
		DatabaseName:     "prod",
		Category:         "stale_stats",
		Severity:         SeverityWarning,
		ObjectType:       "table",
		ObjectIdentifier: "public.orders",
		Title:            "Stats are stale",
		Recommendation:   "Run ANALYZE on public.orders",
		RecommendedSQL:   "ANALYZE public.orders",
		Detail: map[string]any{
			"n_mod_since_analyze": float64(200000),
			"last_analyze_age":    "72h",
		},
	}

	got := ProjectFinding(f)

	if got.SourceType != SourceFindingType {
		t.Fatalf("SourceType = %q", got.SourceType)
	}
	if got.ActionCandidates[0].ActionType != "analyze_table" {
		t.Fatalf("ActionType = %q", got.ActionCandidates[0].ActionType)
	}
	if got.ActionCandidates[0].RiskTier != "safe" {
		t.Fatalf("RiskTier = %q", got.ActionCandidates[0].RiskTier)
	}
	if got.WhyNow == "not urgent" {
		t.Fatalf("WhyNow was not populated from stale-stat detail")
	}
}

func TestProjectFindingInformationalWhenNoRemediation(t *testing.T) {
	f := SourceFinding{
		ID:               "99",
		DatabaseName:     "prod",
		Category:         "schema_lint:lint_serial_usage",
		Severity:         SeverityInfo,
		ObjectType:       "column",
		ObjectIdentifier: "public.orders.id",
		Title:            "Legacy serial usage",
		Recommendation:   "Prefer identity columns for new schema.",
	}

	got := ProjectFinding(f)

	if len(got.ActionCandidates) != 0 {
		t.Fatalf("expected no action candidates, got %d", len(got.ActionCandidates))
	}
	if got.State != StateOpen {
		t.Fatalf("State = %q", got.State)
	}
}
```

- [ ] **Step 2: Run targeted tests and verify RED**

Run:

```bash
go test -count=1 ./sidecar/internal/cases
```

Expected: FAIL because `ProjectFinding` is undefined.

- [ ] **Step 3: Implement finding projection**

Create `sidecar/internal/cases/projector.go`:

```go
package cases

import (
	"fmt"
	"strings"
	"time"
)

func ProjectFinding(f SourceFinding) Case {
	evidence := []Evidence{{
		Type:    "finding",
		Summary: f.Title,
		Fields:  f.Detail,
	}}
	actions := actionCandidatesForFinding(f)
	whyNow := whyNowForFinding(f)

	return NewCase(CaseInput{
		SourceType:       SourceFindingType,
		SourceID:         f.ID,
		DatabaseName:     f.DatabaseName,
		IdentityKey:      IdentityKeyForFinding(f),
		Subsystem:        subsystemForFinding(f),
		Category:         f.Category,
		Title:            f.Title,
		Summary:          f.Recommendation,
		Severity:         f.Severity,
		ImpactScore:      impactScoreForSeverity(f.Severity),
		UrgencyScore:     urgencyForFinding(f),
		AffectedObjects:  affectedObjectsForFinding(f),
		AffectedQueries:  affectedQueriesForFinding(f),
		Evidence:         evidence,
		Hypothesis:       f.Recommendation,
		WhyNow:           whyNow,
		ActionCandidates: actions,
	})
}

func actionCandidatesForFinding(f SourceFinding) []ActionCandidate {
	sql := strings.TrimSpace(f.RecommendedSQL)
	if sql == "" {
		return nil
	}
	actionType := actionTypeForSQL(sql)
	if actionType == "" {
		return nil
	}
	expires := time.Now().UTC().Add(24 * time.Hour)
	return []ActionCandidate{{
		ActionType:       actionType,
		RiskTier:         riskForActionType(actionType),
		Confidence:       0.70,
		ProposedSQL:      sql,
		ExpiresAt:        &expires,
		OutputModes:      []string{"queue_for_approval", "generate_pr_or_script"},
		RollbackClass:    rollbackClassForAction(actionType),
		VerificationPlan: verificationPlanForAction(actionType),
	}}
}

func actionTypeForSQL(sql string) string {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(upper, "ANALYZE "):
		return "analyze_table"
	case strings.HasPrefix(upper, "CREATE INDEX CONCURRENTLY "):
		return "create_index_concurrently"
	case strings.HasPrefix(upper, "DROP INDEX "):
		return "drop_unused_index"
	default:
		return ""
	}
}

func riskForActionType(actionType string) string {
	switch actionType {
	case "analyze_table":
		return "safe"
	case "create_index_concurrently", "drop_unused_index":
		return "moderate"
	default:
		return "high"
	}
}

func rollbackClassForAction(actionType string) string {
	switch actionType {
	case "analyze_table":
		return "no_rollback_needed"
	case "create_index_concurrently", "drop_unused_index":
		return "reversible"
	default:
		return "forward_fix_only"
	}
}

func verificationPlanForAction(actionType string) []string {
	switch actionType {
	case "analyze_table":
		return []string{
			"verify last_analyze or analyze_count advanced",
			"rerun analyzer and confirm stale-stat case no longer fires",
			"compare planner row-estimate error for tracked queries",
		}
	default:
		return []string{"rerun analyzer and verify case state"}
	}
}

func subsystemForFinding(f SourceFinding) string {
	if strings.HasPrefix(f.Category, "schema_lint:") {
		return "schema"
	}
	if f.ObjectType == "query" {
		return "query"
	}
	return "database"
}

func impactScoreForSeverity(sev string) int {
	switch sev {
	case SeverityCritical:
		return 90
	case SeverityWarning:
		return 60
	default:
		return 25
	}
}

func urgencyForFinding(f SourceFinding) int {
	base := impactScoreForSeverity(f.Severity)
	if strings.Contains(strings.ToLower(f.Category), "stale") {
		return minInt(100, base+15)
	}
	return base
}

func whyNowForFinding(f SourceFinding) string {
	if v, ok := f.Detail["n_mod_since_analyze"]; ok {
		return fmt.Sprintf("table changed since last analyze: %v rows", v)
	}
	if f.Severity == SeverityCritical {
		return "critical severity requires immediate review"
	}
	return "not urgent"
}

func affectedObjectsForFinding(f SourceFinding) []string {
	if f.ObjectIdentifier == "" || f.ObjectType == "query" {
		return nil
	}
	return []string{f.ObjectIdentifier}
}

func affectedQueriesForFinding(f SourceFinding) []string {
	if f.ObjectType != "query" {
		return nil
	}
	if norm, ok := f.Detail["normalized_query"].(string); ok && norm != "" {
		return []string{norm}
	}
	if f.ObjectIdentifier != "" {
		return []string{f.ObjectIdentifier}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

- [ ] **Step 4: Run targeted tests and verify GREEN**

Run:

```bash
go test -count=1 ./sidecar/internal/cases
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sidecar/internal/cases/projector.go sidecar/internal/cases/projector_test.go
git commit -m "feat(cases): project findings into cases"
```

---

## Task 3: Ephemeral Resolution And Action Expiry Rules

**Files:**
- Modify: `sidecar/internal/cases/case.go`
- Modify: `sidecar/internal/cases/projector.go`
- Modify: `sidecar/internal/cases/projector_test.go`

- [ ] **Step 1: Add failing tests for ephemeral and expiration behavior**

Append to `sidecar/internal/cases/projector_test.go`:

```go
func TestResolveEphemeralWhenEvidenceDisappears(t *testing.T) {
	open := NewCase(CaseInput{
		SourceType:   SourceFindingType,
		SourceID:     "1",
		DatabaseName: "prod",
		IdentityKey:  "finding:prod:lock",
		Title:        "Lock pileup",
		Severity:     SeverityWarning,
		Evidence:     []Evidence{{Type: "lock", Summary: "blocked sessions"}},
		ActionCandidates: []ActionCandidate{{
			ActionType: "cancel_backend",
			RiskTier:   "moderate",
		}},
	})

	got := ResolveIfEvidenceMissing(open, false)

	if got.State != StateResolvedEphemeral {
		t.Fatalf("State = %q, want %q", got.State, StateResolvedEphemeral)
	}
	if len(got.ActionCandidates) != 0 {
		t.Fatalf("expected pending candidates to clear")
	}
}

func TestExpiredActionCannotExecuteWithoutRevalidation(t *testing.T) {
	expired := time.Now().Add(-time.Minute)
	c := ActionCandidate{
		ActionType: "analyze_table",
		RiskTier:   "safe",
		ExpiresAt:  &expired,
	}

	if c.IsExecutable(time.Now()) {
		t.Fatalf("expired action should not be executable")
	}
}
```

- [ ] **Step 2: Run targeted tests and verify RED**

Run:

```bash
go test -count=1 ./sidecar/internal/cases
```

Expected: FAIL because `ResolveIfEvidenceMissing` and `IsExecutable` do not exist.

- [ ] **Step 3: Implement ephemeral and expiration helpers**

Add to `sidecar/internal/cases/case.go`:

```go
func (a ActionCandidate) IsExecutable(now time.Time) bool {
	if a.ExpiresAt == nil {
		return true
	}
	return now.Before(*a.ExpiresAt)
}
```

Add to `sidecar/internal/cases/projector.go`:

```go
func ResolveIfEvidenceMissing(c Case, evidencePresent bool) Case {
	if evidencePresent {
		return c
	}
	c.State = StateResolvedEphemeral
	c.ActionCandidates = nil
	c.WhyNow = "underlying evidence disappeared before action"
	c.UpdatedAt = time.Now().UTC()
	return c
}
```

- [ ] **Step 4: Run targeted tests and verify GREEN**

Run:

```bash
go test -count=1 ./sidecar/internal/cases
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sidecar/internal/cases/case.go sidecar/internal/cases/projector.go sidecar/internal/cases/projector_test.go
git commit -m "feat(cases): handle ephemeral cases and action expiry"
```

---

## Task 4: Case API Endpoints

**Files:**
- Create: `sidecar/internal/api/cases_handlers.go`
- Create: `sidecar/internal/api/cases_handlers_test.go`
- Modify: `sidecar/internal/api/router.go`
- Modify: `sidecar/internal/api/api_test.go`

- [ ] **Step 1: Write failing API tests**

Create `sidecar/internal/api/cases_handlers_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCasesHandlerRejectsBadDatabaseParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/cases?database=bad'db", nil)
	rr := httptest.NewRecorder()

	casesHandler(nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestCasesHandlerEmptyWhenNoFleet(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/v1/cases", nil)
	rr := httptest.NewRecorder()

	casesHandler(nil).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["total"].(float64) != 0 {
		t.Fatalf("total = %v, want 0", body["total"])
	}
}
```

- [ ] **Step 2: Run targeted API tests and verify RED**

Run:

```bash
go test -count=1 ./sidecar/internal/api -run 'TestCasesHandler'
```

Expected: FAIL because `casesHandler` is undefined.

- [ ] **Step 3: Implement additive handlers**

Create `sidecar/internal/api/cases_handlers.go`:

```go
package api

import (
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func casesHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" {
			database = "all"
		}
		if mgr == nil {
			jsonResponse(w, map[string]any{
				"database": database,
				"cases":    []cases.Case{},
				"total":    0,
			})
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		projected, err := queryProjectedCases(r.Context(), mgr, database)
		if err != nil {
			slog.Error("query cases failed", "error", err)
			jsonError(w, "failed to query cases", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{
			"database": database,
			"cases":    projected,
			"total":    len(projected),
		})
	}
}
```

Then add `queryProjectedCases` using existing finding query helpers. Start with findings only:

```go
func queryProjectedCases(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	database string,
) ([]cases.Case, error) {
	filters := findingFilters{Status: "open", Limit: 500}
	pools := poolsForDatabaseSelection(mgr, database)
	out := make([]cases.Case, 0)
	for _, selected := range pools {
		rows, _, err := queryFindings(ctx, selected.pool, filters, selected.name)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			out = append(out, cases.ProjectFinding(sourceFindingFromMap(row, selected.name)))
		}
	}
	return out, nil
}
```

If function or type names differ, adapt to the existing helper shapes in `handlers.go`; keep the handler response contract unchanged.

- [ ] **Step 4: Register routes**

Modify `registerAPIRoutes` in `sidecar/internal/api/router.go` to include:

```go
mux.Handle("GET /api/v1/cases", operatorUp(http.HandlerFunc(casesHandler(mgr))))
```

Use the same role wrapper as findings if one exists in the route group; viewers may read cases if they can read findings.

- [ ] **Step 5: Run targeted API tests and route registration tests**

Run:

```bash
go test -count=1 ./sidecar/internal/api -run 'TestCasesHandler|TestRoutes'
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sidecar/internal/api/cases_handlers.go sidecar/internal/api/cases_handlers_test.go sidecar/internal/api/router.go sidecar/internal/api/api_test.go
git commit -m "feat(api): expose projected cases"
```

---

## Task 5: Shadow Mode Report

**Files:**
- Create: `sidecar/internal/cases/shadow.go`
- Create: `sidecar/internal/cases/shadow_test.go`
- Modify: `sidecar/internal/api/cases_handlers.go`
- Modify: `sidecar/internal/api/cases_handlers_test.go`

- [ ] **Step 1: Write failing Shadow Mode tests**

Create `sidecar/internal/cases/shadow_test.go`:

```go
package cases

import "testing"

func TestShadowReportCountsAutoSafeCandidates(t *testing.T) {
	report := BuildShadowReport([]Case{
		NewCase(CaseInput{
			IdentityKey:  "case-1",
			DatabaseName: "prod",
			Title:        "stale stats",
			Severity:     SeverityWarning,
			Evidence:     []Evidence{{Type: "finding", Summary: "stale"}},
			ActionCandidates: []ActionCandidate{{
				ActionType: "analyze_table",
				RiskTier:   "safe",
			}},
		}),
		NewCase(CaseInput{
			IdentityKey:  "case-2",
			DatabaseName: "prod",
			Title:        "needs DDL",
			Severity:     SeverityWarning,
			Evidence:     []Evidence{{Type: "finding", Summary: "ddl"}},
			ActionCandidates: []ActionCandidate{{
				ActionType:    "ddl_preflight",
				RiskTier:      "high",
				BlockedReason: "requires approval",
			}},
		}),
	})

	if report.TotalCases != 2 {
		t.Fatalf("TotalCases = %d", report.TotalCases)
	}
	if report.WouldAutoResolve != 1 {
		t.Fatalf("WouldAutoResolve = %d", report.WouldAutoResolve)
	}
	if report.RequiresApproval != 1 {
		t.Fatalf("RequiresApproval = %d", report.RequiresApproval)
	}
}
```

- [ ] **Step 2: Run targeted tests and verify RED**

Run:

```bash
go test -count=1 ./sidecar/internal/cases -run TestShadowReport
```

Expected: FAIL because `BuildShadowReport` is undefined.

- [ ] **Step 3: Implement Shadow Mode report**

Create `sidecar/internal/cases/shadow.go`:

```go
package cases

type ShadowReport struct {
	TotalCases        int      `json:"total_cases"`
	WouldAutoResolve  int      `json:"would_auto_resolve"`
	RequiresApproval  int      `json:"requires_approval"`
	Blocked           int      `json:"blocked"`
	EstimatedToilMins int      `json:"estimated_toil_minutes"`
	BlockedReasons    []string `json:"blocked_reasons"`
}

func BuildShadowReport(cases []Case) ShadowReport {
	report := ShadowReport{TotalCases: len(cases)}
	reasons := map[string]bool{}
	for _, c := range cases {
		for _, a := range c.ActionCandidates {
			switch {
			case a.RiskTier == "safe" && a.BlockedReason == "":
				report.WouldAutoResolve++
				report.EstimatedToilMins += estimatedToilForAction(a.ActionType)
			case a.BlockedReason != "":
				report.Blocked++
				reasons[a.BlockedReason] = true
			default:
				report.RequiresApproval++
			}
		}
	}
	for r := range reasons {
		report.BlockedReasons = append(report.BlockedReasons, r)
	}
	return report
}

func estimatedToilForAction(actionType string) int {
	switch actionType {
	case "analyze_table":
		return 15
	default:
		return 30
	}
}
```

- [ ] **Step 4: Add API route**

Add handler in `cases_handlers.go`:

```go
func shadowReportHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" {
			database = "all"
		}
		if mgr == nil {
			jsonResponse(w, cases.BuildShadowReport(nil))
			return
		}
		projected, err := queryProjectedCases(r.Context(), mgr, database)
		if err != nil {
			jsonError(w, "failed to query shadow report", 500)
			return
		}
		jsonResponse(w, cases.BuildShadowReport(projected))
	}
}
```

Register:

```go
mux.Handle("GET /api/v1/shadow-report", operatorUp(http.HandlerFunc(shadowReportHandler(mgr))))
```

- [ ] **Step 5: Run targeted tests**

Run:

```bash
go test -count=1 ./sidecar/internal/cases ./sidecar/internal/api -run 'TestShadowReport|TestCasesHandler'
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sidecar/internal/cases/shadow.go sidecar/internal/cases/shadow_test.go sidecar/internal/api/cases_handlers.go sidecar/internal/api/cases_handlers_test.go sidecar/internal/api/router.go
git commit -m "feat(cases): add shadow mode report"
```

---

## Task 6: Typed `analyze_table` Action Contract

**Files:**
- Create: `sidecar/internal/executor/action_contract.go`
- Create: `sidecar/internal/executor/action_contract_test.go`
- Modify: `sidecar/internal/executor/analyze.go`

- [ ] **Step 1: Write failing action contract tests**

Create `sidecar/internal/executor/action_contract_test.go`:

```go
package executor

import "testing"

func TestAnalyzeTableContractIsExecutableAndVerified(t *testing.T) {
	c := AnalyzeTableContract()

	if c.ActionType != "analyze_table" {
		t.Fatalf("ActionType = %q", c.ActionType)
	}
	if c.BaseRiskTier != "safe" {
		t.Fatalf("BaseRiskTier = %q", c.BaseRiskTier)
	}
	if len(c.Prechecks) == 0 {
		t.Fatalf("expected prechecks")
	}
	if len(c.PostChecks) == 0 {
		t.Fatalf("expected post-checks")
	}
	if c.RollbackClass != "no_rollback_needed" {
		t.Fatalf("RollbackClass = %q", c.RollbackClass)
	}
}

func TestActionContractRequiresPostChecks(t *testing.T) {
	c := ActionContract{ActionType: "bad_action", BaseRiskTier: "safe"}

	if err := c.Validate(); err == nil {
		t.Fatalf("expected validation error")
	}
}
```

- [ ] **Step 2: Run targeted tests and verify RED**

Run:

```bash
go test -count=1 ./sidecar/internal/executor -run 'TestAnalyzeTableContract|TestActionContract'
```

Expected: FAIL because `ActionContract` is undefined.

- [ ] **Step 3: Implement action contract**

Create `sidecar/internal/executor/action_contract.go`:

```go
package executor

import "errors"

type ActionContract struct {
	ActionType          string
	BaseRiskTier        string
	ProviderSupport     []string
	RequiredPermissions []string
	Prechecks           []string
	Guardrails          []string
	ExecutionPlan       []string
	SuccessCriteria     []string
	PostChecks          []string
	RollbackClass       string
	Cooldown            string
	AuditFields         []string
}

func (c ActionContract) Validate() error {
	if c.ActionType == "" || c.BaseRiskTier == "" {
		return errors.New("action contract missing type or risk tier")
	}
	if len(c.PostChecks) == 0 {
		return errors.New("action contract missing post-checks")
	}
	if c.RollbackClass == "" {
		return errors.New("action contract missing rollback class")
	}
	return nil
}

func AnalyzeTableContract() ActionContract {
	return ActionContract{
		ActionType:      "analyze_table",
		BaseRiskTier:    "safe",
		ProviderSupport: []string{"postgres", "rds", "aurora", "cloud-sql", "alloydb"},
		RequiredPermissions: []string{
			"table ownership or ANALYZE privilege",
		},
		Prechecks: []string{
			"table exists",
			"table is not excluded by policy",
			"emergency stop is not active",
			"stale-stat evidence exists",
			"cooldown window has elapsed",
		},
		Guardrails: []string{
			"dedicated connection",
			"statement_timeout",
			"per-table cooldown",
			"fleet analyze semaphore",
			"per-cluster safe-action concurrency limit",
		},
		ExecutionPlan: []string{"ANALYZE qualified_table"},
		SuccessCriteria: []string{
			"last_analyze advances or analyze_count increases",
			"stale-stat case no longer fires",
		},
		PostChecks: []string{
			"verify last_analyze or analyze_count changed",
			"rerun analyzer",
			"compare planner row-estimate error where available",
		},
		RollbackClass: "no_rollback_needed",
		Cooldown:      "configured analyze cooldown",
		AuditFields: []string{
			"table",
			"prior_last_analyze",
			"new_last_analyze",
			"prior_estimate_error",
			"post_action_estimate_error",
			"case_id",
		},
	}
}
```

- [ ] **Step 4: Run targeted executor tests**

Run:

```bash
go test -count=1 ./sidecar/internal/executor -run 'TestAnalyzeTableContract|TestActionContract|TestIsAnalyzeStatement|TestCheckAnalyzeCooldown'
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sidecar/internal/executor/action_contract.go sidecar/internal/executor/action_contract_test.go
git commit -m "feat(executor): define analyze action contract"
```

---

## Task 7: Cases UI And Route Compatibility

**Files:**
- Create: `sidecar/web/src/pages/CasesPage.jsx`
- Create: `sidecar/web/src/pages/CasesPage.test.jsx`
- Modify: `sidecar/web/src/App.jsx`
- Modify: `sidecar/web/src/components/Layout.jsx`
- Modify: `sidecar/web/src/components/CommandPalette.jsx`

- [ ] **Step 1: Write failing Cases UI test**

Create `sidecar/web/src/pages/CasesPage.test.jsx`:

```jsx
import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { CasesPage } from './CasesPage'

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    data: {
      cases: [{
        case_id: 'case-1',
        title: 'Stats are stale',
        severity: 'warning',
        state: 'open',
        impact_score: 60,
        urgency_score: 75,
        why_now: 'table changed since last analyze',
        action_candidates: [{
          action_type: 'analyze_table',
          risk_tier: 'safe',
        }],
      }],
      total: 1,
    },
    loading: false,
    error: null,
    refetch: vi.fn(),
  }),
}))

describe('CasesPage', () => {
  it('shows case next step and why now', () => {
    render(<CasesPage database="all" />)

    expect(screen.getByText('Stats are stale')).toBeInTheDocument()
    expect(screen.getByText('table changed since last analyze')).toBeInTheDocument()
    expect(screen.getByText('analyze_table')).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run UI test and verify RED**

Run:

```bash
cd sidecar/web
npm test -- CasesPage.test.jsx
```

Expected: FAIL because `CasesPage` does not exist.

- [ ] **Step 3: Implement CasesPage**

Create `sidecar/web/src/pages/CasesPage.jsx`:

```jsx
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'

function dbParam(database) {
  return database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}`
    : ''
}

function nextStep(caseRow) {
  const candidate = caseRow.action_candidates?.[0]
  if (!candidate) return 'No action proposed'
  if (candidate.blocked_reason) return candidate.blocked_reason
  return candidate.action_type
}

export function CasesPage({ database }) {
  const { data, loading, error } = useAPI(
    `/api/v1/cases${dbParam(database)}`,
    30000,
  )

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} />

  const cases = data?.cases || []

  return (
    <div className="space-y-4" data-testid="cases-page">
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          DBA Cases
        </h2>
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          {cases.length} active cases ranked by urgency and actionability.
        </p>
      </div>

      <div className="space-y-2">
        {cases.map(c => (
          <article key={c.case_id}
            className="rounded border p-3"
            style={{
              background: 'var(--bg-card)',
              borderColor: 'var(--border)',
            }}>
            <div className="flex items-start justify-between gap-3">
              <div>
                <h3 className="font-medium"
                  style={{ color: 'var(--text-primary)' }}>
                  {c.title}
                </h3>
                <p className="text-sm mt-1"
                  style={{ color: 'var(--text-secondary)' }}>
                  {c.why_now || 'not urgent'}
                </p>
              </div>
              <span className="text-xs uppercase"
                style={{ color: 'var(--text-secondary)' }}>
                {c.severity}
              </span>
            </div>
            <div className="mt-3 flex flex-wrap gap-2 text-xs"
              style={{ color: 'var(--text-secondary)' }}>
              <span>State: {c.state}</span>
              <span>Impact: {c.impact_score}</span>
              <span>Urgency: {c.urgency_score}</span>
              <span>Next: {nextStep(c)}</span>
            </div>
          </article>
        ))}
      </div>
    </div>
  )
}
```

- [ ] **Step 4: Wire route and nav alias**

Modify `sidecar/web/src/App.jsx`:

```jsx
import { CasesPage } from './pages/CasesPage'
```

Route mapping:

```jsx
case '/cases':
case '/findings':
  return { title: 'Cases',
    node: <CasesPage database={selectedDB} user={user} /> }
```

Keep `#/findings` as compatibility alias until e2e tests are updated.

Modify `sidecar/web/src/components/Layout.jsx` nav groups to target:

```jsx
const NAV_GROUPS = [
  {
    heading: 'Operate',
    items: [
      { path: '#/', icon: Home, label: 'Overview',
        tid: 'nav-dashboard' },
      { path: '#/cases', icon: AlertTriangle,
        label: 'Cases', tid: 'nav-cases' },
      { path: '#/actions', icon: Activity, label: 'Actions',
        tid: 'nav-actions' },
      { path: '#/manage-databases', icon: Server,
        label: 'Fleet', admin: true, tid: 'nav-databases' },
      { path: '#/settings', icon: Settings, label: 'Settings',
        admin: true, tid: 'nav-settings' },
    ],
  },
]
```

- [ ] **Step 5: Run UI tests**

Run:

```bash
cd sidecar/web
npm test -- CasesPage.test.jsx
npm test -- SettingsPage.test.jsx QueryHintsPage.test.jsx
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sidecar/web/src/pages/CasesPage.jsx sidecar/web/src/pages/CasesPage.test.jsx sidecar/web/src/App.jsx sidecar/web/src/components/Layout.jsx sidecar/web/src/components/CommandPalette.jsx
git commit -m "feat(web): add cases route and compact navigation"
```

---

## Task 8: Shadow Mode UI

**Files:**
- Create: `sidecar/web/src/pages/ShadowModePage.jsx`
- Create: `sidecar/web/src/pages/ShadowModePage.test.jsx`
- Modify: `sidecar/web/src/pages/SettingsPage.jsx`

- [ ] **Step 1: Write failing UI test**

Create `sidecar/web/src/pages/ShadowModePage.test.jsx`:

```jsx
import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ShadowModePage } from './ShadowModePage'

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    data: {
      total_cases: 14,
      would_auto_resolve: 12,
      requires_approval: 2,
      blocked: 1,
      estimated_toil_minutes: 360,
      blocked_reasons: ['requires approval'],
    },
    loading: false,
    error: null,
  }),
}))

describe('ShadowModePage', () => {
  it('shows avoided toil and auto-safe preview', () => {
    render(<ShadowModePage database="all" />)

    expect(screen.getByText('14')).toBeInTheDocument()
    expect(screen.getByText('12')).toBeInTheDocument()
    expect(screen.getByText('360 min')).toBeInTheDocument()
  })
})
```

- [ ] **Step 2: Run UI test and verify RED**

Run:

```bash
cd sidecar/web
npm test -- ShadowModePage.test.jsx
```

Expected: FAIL because component does not exist.

- [ ] **Step 3: Implement ShadowModePage**

Create `sidecar/web/src/pages/ShadowModePage.jsx`:

```jsx
import { useAPI } from '../hooks/useAPI'
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'

function dbParam(database) {
  return database && database !== 'all'
    ? `?database=${encodeURIComponent(database)}`
    : ''
}

function Stat({ label, value }) {
  return (
    <div className="rounded border p-3"
      style={{ background: 'var(--bg-card)', borderColor: 'var(--border)' }}>
      <div className="text-2xl font-semibold"
        style={{ color: 'var(--text-primary)' }}>
        {value}
      </div>
      <div className="text-xs mt-1"
        style={{ color: 'var(--text-secondary)' }}>
        {label}
      </div>
    </div>
  )
}

export function ShadowModePage({ database }) {
  const { data, loading, error } = useAPI(
    `/api/v1/shadow-report${dbParam(database)}`,
    30000,
  )

  if (loading) return <LoadingSpinner />
  if (error) return <ErrorBanner message={error} />

  return (
    <section className="space-y-4" data-testid="shadow-mode-report">
      <div>
        <h2 className="text-sm font-semibold"
          style={{ color: 'var(--text-primary)' }}>
          Shadow Mode
        </h2>
        <p className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          What pg_sage would have handled under auto-safe policy.
        </p>
      </div>
      <div className="grid gap-3 md:grid-cols-4">
        <Stat label="Cases detected" value={data?.total_cases ?? 0} />
        <Stat label="Would auto-resolve" value={data?.would_auto_resolve ?? 0} />
        <Stat label="Needs approval" value={data?.requires_approval ?? 0} />
        <Stat label="Avoided toil" value={`${data?.estimated_toil_minutes ?? 0} min`} />
      </div>
      {(data?.blocked_reasons || []).length > 0 && (
        <div className="text-sm" style={{ color: 'var(--text-secondary)' }}>
          Blocked: {data.blocked_reasons.join(', ')}
        </div>
      )}
    </section>
  )
}
```

- [ ] **Step 4: Embed Shadow Mode in Settings**

In `SettingsPage.jsx`, add the component to the autonomy/policy section or as a new tab label `Shadow Mode`. Keep it under Settings, not top-level nav.

- [ ] **Step 5: Run UI tests**

Run:

```bash
cd sidecar/web
npm test -- ShadowModePage.test.jsx SettingsPage.test.jsx
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sidecar/web/src/pages/ShadowModePage.jsx sidecar/web/src/pages/ShadowModePage.test.jsx sidecar/web/src/pages/SettingsPage.jsx
git commit -m "feat(web): add shadow mode report"
```

---

## Task 9: Actions Timeline Read Shape

**Files:**
- Modify: `sidecar/internal/api/action_handlers.go`
- Modify: `sidecar/internal/api/action_handlers_test.go`
- Modify: `sidecar/web/src/pages/Actions.jsx`

- [ ] **Step 1: Add failing test for timeline response**

Add to `action_handlers_test.go`:

```go
func TestActionsTimelineResponseIncludesStatusRiskAndVerification(t *testing.T) {
	row := map[string]any{
		"id":                  1,
		"status":              "pending",
		"action_type":         "analyze_table",
		"risk_tier":           "safe",
		"verification_status": "not_started",
	}

	got := actionTimelineMap(row)

	for _, key := range []string{"id", "status", "action_type", "risk_tier", "verification_status"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s in timeline map", key)
		}
	}
}
```

- [ ] **Step 2: Run targeted test and verify RED**

Run:

```bash
go test -count=1 ./sidecar/internal/api -run TestActionsTimelineResponse
```

Expected: FAIL because `actionTimelineMap` is undefined.

- [ ] **Step 3: Implement timeline map helper**

Add to `action_handlers.go`:

```go
func actionTimelineMap(row map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{
		"id",
		"case_id",
		"finding_id",
		"status",
		"action_type",
		"risk_tier",
		"proposed_sql",
		"actor",
		"verification_status",
		"rollback_status",
		"created_at",
		"approved_at",
		"executed_at",
		"verified_at",
		"expires_at",
	} {
		if v, ok := row[key]; ok {
			out[key] = v
		}
	}
	return out
}
```

Wire this helper into an additive timeline endpoint only if existing list endpoints cannot provide the shape:

```go
mux.Handle("GET /api/v1/actions/timeline", operatorUp(http.HandlerFunc(actionsTimelineHandler(deps))))
```

- [ ] **Step 4: Update Actions UI default labels**

Keep current tabs working. Add timeline fields to row rendering when present:

```jsx
const status = row.status || row.action_status || 'unknown'
const risk = row.risk_tier || row.action_risk || row.risk || 'unknown'
const verification = row.verification_status || row.status || 'not_started'
```

- [ ] **Step 5: Run targeted tests**

Run:

```bash
go test -count=1 ./sidecar/internal/api -run 'TestActionsTimelineResponse|Test.*Action'
cd sidecar/web
npm test -- Actions
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sidecar/internal/api/action_handlers.go sidecar/internal/api/action_handlers_test.go sidecar/web/src/pages/Actions.jsx
git commit -m "feat(actions): expose timeline fields"
```

---

## Task 10: E2E Navigation And Compatibility

**Files:**
- Create: `e2e/cases.spec.ts`
- Create: `e2e/autonomous-first-slice.spec.ts`
- Modify: `e2e/navigation.spec.ts`

- [ ] **Step 1: Add failing Cases e2e test**

Create `e2e/cases.spec.ts`:

```ts
import { expect, test } from '@playwright/test'
import { login } from './helpers'

test('Cases page loads and old findings route aliases to cases', async ({ page }) => {
  await login(page)

  await page.goto('/#/cases')
  await expect(page.getByRole('heading', { name: 'Cases' })).toBeVisible()
  await expect(page.getByTestId('cases-page')).toBeVisible()

  await page.goto('/#/findings')
  await expect(page.getByRole('heading', { name: 'Cases' })).toBeVisible()
})
```

Create `e2e/autonomous-first-slice.spec.ts`:

```ts
import { expect, test } from '@playwright/test'
import { login } from './helpers'

test('primary nav is contracted to autonomous DBA surfaces', async ({ page }) => {
  await login(page)
  await page.goto('/#/users')

  await expect(page.getByTestId('nav-dashboard')).toContainText('Overview')
  await expect(page.getByTestId('nav-cases')).toContainText('Cases')
  await expect(page.getByTestId('nav-actions')).toContainText('Actions')
  await expect(page.getByTestId('nav-databases')).toContainText('Fleet')
  await expect(page.getByTestId('nav-settings')).toContainText('Settings')

  await expect(page.getByTestId('nav-query-hints')).toHaveCount(0)
  await expect(page.getByTestId('nav-schema-health')).toHaveCount(0)
  await expect(page.getByTestId('nav-incidents')).toHaveCount(0)
})
```

- [ ] **Step 2: Run e2e tests and verify RED**

Run:

```bash
cd e2e
npx playwright test cases.spec.ts autonomous-first-slice.spec.ts
```

Expected: FAIL until routes/nav are fully wired.

- [ ] **Step 3: Fix route/nav issues only**

Make only the smallest frontend changes needed to satisfy the e2e expectations. Do not remove old page components from disk in this task.

- [ ] **Step 4: Run e2e tests and verify GREEN**

Run:

```bash
cd e2e
npx playwright test cases.spec.ts autonomous-first-slice.spec.ts navigation.spec.ts
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add e2e/cases.spec.ts e2e/autonomous-first-slice.spec.ts e2e/navigation.spec.ts sidecar/web/src/App.jsx sidecar/web/src/components/Layout.jsx
git commit -m "test(e2e): verify autonomous DBA navigation"
```

---

## Task 11: Full Verification

**Files:**
- No production file changes unless verification finds a bug.

- [ ] **Step 1: Run Go coverage suite**

Run:

```bash
go test -cover -count=1 ./...
```

Expected: PASS. Report total passed/failed/skipped and per-package coverage gaps per `AGENTS.md`.

- [ ] **Step 2: Check for silent skips**

Run:

```bash
go test -count=1 ./... 2>&1 | tee tasks/autonomous-dba-go-test-output.txt
grep -Ei "SKIP|TODO|PENDING" tasks/autonomous-dba-go-test-output.txt
```

Expected: no unexplained skips. Any skip must be listed in the final test report.

- [ ] **Step 3: Run web tests**

Run:

```bash
cd sidecar/web
npm test -- --run
npm run build
```

Expected: PASS and build succeeds.

- [ ] **Step 4: Run focused Playwright e2e**

Run:

```bash
cd e2e
npx playwright test cases.spec.ts autonomous-first-slice.spec.ts actions.spec.ts settings.spec.ts navigation.spec.ts
```

Expected: PASS.

- [ ] **Step 5: Browser verification**

Use the in-app browser at `http://127.0.0.1:18085`:

```text
CHECK-01: Overview route loads
CHECK-02: Cases route loads
CHECK-03: Old #/findings route aliases to Cases
CHECK-04: Actions route shows pending/executed data
CHECK-05: Fleet route replaces Databases label
CHECK-06: Settings contains Shadow Mode report
CHECK-07: Removed top-level routes are absent from nav
CHECK-08: No console errors on Overview, Cases, Actions, Fleet, Settings
```

- [ ] **Step 6: Commit verification docs if generated**

```bash
git add tasks/autonomous-dba-go-test-output.txt
git commit -m "test: verify autonomous DBA first slice"
```

Only commit generated verification artifacts if the repo convention accepts them. Otherwise leave them untracked and summarize results.

---

## Rollback Plan

If the UI contraction causes regressions:

1. Keep backend case APIs; they are additive.
2. Restore old `NAV_GROUPS` in `Layout.jsx`.
3. Keep `#/cases` route available.
4. Re-run `e2e/navigation.spec.ts`.
5. File the nav contraction as a separate follow-up.

If the Case projection causes API errors:

1. Leave old `/api/v1/findings`, `/api/v1/query-hints`, `/api/v1/forecasts`, and `/api/v1/incidents` untouched.
2. Disable only `/api/v1/cases` route registration.
3. Keep package tests for cases and fix projection offline.

If `analyze_table` contract conflicts with existing executor behavior:

1. Keep contract as metadata only.
2. Do not gate existing executor behavior until targeted tests prove compatibility.
3. Reconcile executor behavior in a separate PR.

---

## Spec Coverage Checklist

- [ ] Case projection API maps existing findings into cases.
- [ ] Cases include identity key, evidence, severity, state, why-now, next step.
- [ ] Ephemeral resolution and action expiration are represented.
- [ ] Shadow Mode reports would-have-acted value.
- [ ] `analyze_table` typed action contract has post-checks and no-rollback-needed semantics.
- [ ] Navigation contracts toward Overview, Cases, Actions, Fleet, Settings.
- [ ] Old findings route remains compatible during transition.
- [ ] Admin plumbing is not a primary new route.
- [ ] Tests prove emergency stop and expired-action behavior before direct executor enforcement expands.

---

## Known Deferred Work

- DDL preflight implementation.
- PR/script generation output.
- Incident playbook execution.
- Full action confidence scoring.
- Full provider capability matrix.
- Storage migration from findings/actions to first-class cases/actions tables.
- Removing old page components from disk after route compatibility window.

