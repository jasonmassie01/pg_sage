package lakebaselive

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/databricksauth"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

var providerProbeSQLState = regexp.MustCompile(`SQLSTATE ([0-9A-Z]{5})`)

func providerProbeSelection(raw string) ([]probes.ID, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("explicit SAGE_PROVIDER_PROBE_IDS allowlist required")
	}
	var ids []probes.ID
	seen := map[probes.ID]bool{}
	for _, value := range strings.Split(raw, ",") {
		id := probes.ID(strings.TrimSpace(value))
		scope := probes.SourceScope(id)
		_, regular := probes.Catalog().Spec(id)
		_, action := probes.ActionRegistry().Spec(id)
		if seen[id] || (!regular && !action) || (scope != "cluster" && scope != "filesystem") {
			return nil, fmt.Errorf("invalid, duplicate or outside provider-probe boundary: %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

func TestProviderProbeSelection(t *testing.T) {
	for _, raw := range []string{"", " ", "lock_chains,lock_chains", "stat_database", "autovacuum_cancellations", "unknown", "lock_chains,wal_directory;SELECT 1"} {
		if _, err := providerProbeSelection(raw); err == nil {
			t.Fatalf("unsafe provider probe selection accepted: %q", raw)
		}
	}
	ids, err := providerProbeSelection("lock_chains, wal_directory")
	if err != nil || len(ids) != 2 || ids[0] != probes.LockChains || ids[1] != probes.WALDirectoryProbe {
		t.Fatalf("valid explicit read-only sources rejected: %v %v", ids, err)
	}
}

// This separate opt-in tests the platform with a trusted, exact allowlist.
// It never changes pg_sage's default database-scoped production policy, grants
// a role, invokes an action, starts a background collector, or logs probe rows.
func TestLakebaseProviderProbeCompatibility(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE_PROVIDER_PROBES") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE_PROVIDER_PROBES=1 and exact SAGE_PROVIDER_PROBE_IDS for isolated branch provider reads")
	}
	ids, err := providerProbeSelection(os.Getenv("SAGE_PROVIDER_PROBE_IDS"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SAGE_LAKEBASE_ENDPOINT") != "projects/search/branches/pg-sage-test/endpoints/primary" {
		t.Fatal("provider reads require the exact isolated test endpoint")
	}
	p, ctx := livePool(t)
	resource, err := databricksauth.ResolveTelemetryResource(ctx, p.Config().ConnConfig.Host, os.Getenv("SAGE_LAKEBASE_ENDPOINT"))
	if err != nil {
		t.Fatal(err)
	}
	policy := &probes.ReadPolicy{Version: 1, Scope: "cluster", Database: "pg_sage_test", Binding: probes.PoolBinding(p), Allowed: ids}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	conn, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var pid int32
	var started time.Time
	if err := conn.QueryRow(ctx, "SELECT pid,backend_start FROM pg_stat_activity WHERE pid=pg_backend_pid()").Scan(&pid, &started); err != nil {
		t.Fatal(err)
	}
	report := struct {
		StartedAt  time.Time                          `json:"started_at"`
		FinishedAt time.Time                          `json:"finished_at"`
		Resource   databricksauth.TelemetryResource   `json:"api_verified_resource"`
		PolicyHash string                             `json:"policy_hash"`
		Results    []providerProbeCompatibilityResult `json:"results"`
	}{StartedAt: time.Now().UTC(), Resource: resource, PolicyHash: policy.Hash()}
	t.Cleanup(func() {
		report.FinishedAt = time.Now().UTC()
		writeProviderCompatibilityReport(t, report, "SAGE_PROVIDER_PROBE_REPORT")
	})
	for _, id := range ids {
		t.Run(string(id), func(t *testing.T) {
			registry := probes.Catalog()
			if _, ok := probes.ActionRegistry().Spec(id); ok {
				registry = probes.ActionRegistry()
			}
			spec, _ := registry.Spec(id)
			args := probes.Args{}
			if spec.Args == probes.ArgsBackend {
				args.PID, args.BackendStart = pid, started
			}
			if spec.Args == probes.ArgsWindow {
				args.Window = time.Minute
			}
			result := probes.NewRunner(p, registry, nil, policy).Run(ctx, id, args)
			entry := providerProbeCompatibilityResult{Source: providerProbeCompatibilitySource{ID: id, Scope: probes.SourceScope(id), Eligible: policy.Allows(id), Status: result.Status, Reason: result.Reason, Version: result.Version, Rows: len(result.Rows), ElapsedMS: result.ElapsedMS}, RequiredRoles: spec.Requires, Extension: spec.Extension, RunnerAttempts: result.Timing.Attempts, FailurePhase: result.Phase, ProbeSQLExecution: "unverified", Classification: "bounded_probe_failure"}
			if match := providerProbeSQLState.FindStringSubmatch(result.Error); len(match) == 2 {
				entry.SQLState = match[1]
			}
			switch {
			case result.Status == probes.StatusOK || result.Status == probes.StatusEmpty:
				entry.Classification = "query_supported_on_observed_configuration"
				entry.ProbeSQLExecution = "confirmed_completed"
			case result.Reason == probes.ReasonMissingRole:
				entry.Classification = "role_prerequisite_refused_before_probe_sql"
				entry.ProbeSQLExecution = "not_reached_preflight"
			case result.Reason == "extension_not_installed":
				entry.Classification = "extension_prerequisite_refused_before_probe_sql"
				entry.ProbeSQLExecution = "not_reached_preflight"
			case result.Reason == probes.ReasonPolicyDenied:
				entry.Classification = "test_authority_invalid"
				entry.ProbeSQLExecution = "not_reached_policy"
			case entry.SQLState == "42501":
				entry.Classification = "provider_role_restriction"
			case result.Status == probes.StatusUnsupported:
				entry.Classification = "platform_schema_or_function_gap"
			case result.Reason == "statement_timeout" || result.Reason == "lock_timeout" || result.Reason == "deadline_exceeded" || result.Reason == "concurrency_limit":
				entry.Classification = "execution_budget_failure"
			}
			report.Results = append(report.Results, entry)
			t.Logf("status=%s reason=%s SQLSTATE=%s rows=%d attempts=%d", result.Status, result.Reason, entry.SQLState, len(result.Rows), result.Timing.Attempts)
			if result.Reason == probes.ReasonPolicyDenied || result.Timing.Attempts == 0 {
				t.Error("provider test did not reach the platform; do not classify this as platform incompatibility")
			} else if result.Status != probes.StatusOK && result.Status != probes.StatusEmpty {
				t.Error("observed provider read gap under explicit branch-scoped test authority; inspect exact metadata outcome")
			}
		})
	}
}

type providerProbeCompatibilityResult struct {
	Source            providerProbeCompatibilitySource `json:"source"`
	RequiredRoles     []string                         `json:"required_roles,omitempty"`
	Extension         string                           `json:"required_extension,omitempty"`
	SQLState          string                           `json:"sqlstate,omitempty"`
	RunnerAttempts    int                              `json:"runner_attempts"`
	FailurePhase      probes.Phase                     `json:"failure_phase,omitempty"`
	ProbeSQLExecution string                           `json:"probe_sql_execution"`
	Classification    string                           `json:"classification"`
}

// An attempted runner can fail acquiring a connection, reading the server
// version, setting transaction options, or checking prerequisites before the
// catalog statement. Do not infer probe-SQL execution merely from attempts.
type providerProbeCompatibilitySource struct {
	ID        probes.ID     `json:"id"`
	Scope     string        `json:"scope"`
	Eligible  bool          `json:"eligible"`
	Status    probes.Status `json:"status"`
	Reason    string        `json:"reason,omitempty"`
	Version   string        `json:"probe_version"`
	Rows      int           `json:"rows"`
	ElapsedMS int64         `json:"elapsed_ms"`
}
