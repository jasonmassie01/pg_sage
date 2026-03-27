# TDD Workflow for pg_sage (Go)

## Core Principle
Write the test FIRST. Watch it fail. Implement the minimum code. Watch it pass. Refactor. Every feature, every bug fix.

## Workflow

### Step 1: Write the Test

Place tests alongside source: `foo.go` → `foo_test.go`.

```go
// internal/analyzer/rules/index_health_test.go
package rules_test

import (
    "context"
    "testing"

    "github.com/pg-sage/sidecar/internal/analyzer/rules"
    "github.com/pg-sage/sidecar/internal/models"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestDuplicateIndexDetection(t *testing.T) {
    // Arrange — build a snapshot with known duplicate indexes
    snapshot := &models.Snapshot{
        Indexes: []models.IndexStat{
            {Name: "idx_users_email", Table: "users", Columns: []string{"email"}},
            {Name: "idx_users_email_dup", Table: "users", Columns: []string{"email"}},
        },
    }

    // Act
    findings := rules.CheckDuplicateIndexes(snapshot)

    // Assert
    require.Len(t, findings, 1)
    assert.Equal(t, models.SeverityWarning, findings[0].Severity)
    assert.Contains(t, findings[0].Detail, "idx_users_email_dup")
}
```

For integration tests that need a real database, use a build tag:

```go
//go:build integration

package collector_test
```

### Step 2: Verify It Fails

```bash
go test ./internal/analyzer/rules/... -v -run TestDuplicateIndexDetection
```

If it doesn't fail (or fails for the wrong reason), the test is wrong. Fix the test first.

### Step 3: Implement

Write the minimum Go code to make the test pass. Use interfaces for database access so unit tests can mock.

### Step 4: Run the Specific Test

```bash
go test ./internal/analyzer/rules/... -v -run TestDuplicateIndexDetection
```

### Step 5: Run Full Suite

```bash
# Unit tests (fast)
go test ./... -v -short -race

# Integration tests (needs Docker Postgres running)
go test ./... -v -tags=integration -race
```

### Step 6: Lint

```bash
golangci-lint run ./...
```

## What to Test per Feature Type

### New rule (Tier 1)
1. Happy path — detects the condition in synthetic snapshot data
2. No-match — does NOT fire when condition is absent
3. Edge cases — empty tables, zero rows, NULL values
4. Severity assignment — correct severity for different thresholds
5. Finding metadata — detail message is useful and actionable

### New action (Tier 3)
1. Trust gating — blocked at OBSERVATION, allowed at correct trust level
2. Execution — action does what it claims against real DB (integration test)
3. Rollback metadata — rollback SQL is captured correctly
4. Circuit breaker — action respects breaker state
5. Logging — action_log entry is complete

### New LLM provider
1. Request formatting — correct HTTP body for the provider's API
2. Response parsing — handles successful response
3. Error handling — handles 4xx, 5xx, timeout, malformed JSON
4. Circuit breaker — wraps provider correctly

### New SQL query (collector)
1. Runs without error against PG 14, 15, 16, 17
2. Returns expected columns
3. Handles empty results
4. Handles permission errors gracefully

## Mocking Database Access

Define interfaces at the consumer. Mock with testify:

```go
// internal/analyzer/analyzer.go
type SnapshotSource interface {
    LatestSnapshot(ctx context.Context) (*models.Snapshot, error)
}

// internal/analyzer/analyzer_test.go
type mockSnapshotSource struct {
    mock.Mock
}

func (m *mockSnapshotSource) LatestSnapshot(ctx context.Context) (*models.Snapshot, error) {
    args := m.Called(ctx)
    return args.Get(0).(*models.Snapshot), args.Error(1)
}
```

## Red Flags

- Writing Go code without a corresponding `_test.go` file → write the test first
- Test passes before implementation exists → test is wrong
- Test requires a live database but isn't tagged `integration` → fix the tag
- Using `time.Sleep` in tests → use channels, contexts, or testify's `Eventually`
- Mocking too much → if you're mocking 5 things, the function does too much; refactor
