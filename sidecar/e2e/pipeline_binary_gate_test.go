//go:build e2e

// executor-10: what the trust gate decided for each Layer A seed. The
// full-binary test once failed with zero actions and no reason in sight;
// the run now ends with every seeded object's sage.decision rows and the
// trust ledger levels, CHECK-A11 refuses a queue verdict without a known
// reason, and two variants pin the documented withholds (a build without
// parse-tree validation, a SAFE tier switched off) by their reasons.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// assumeSQLASTTag builds the binary so its gate treats the parse-tree SQL
// layer as present (cmd/pg_sage_sidecar/sqlast_assumed_e2e.go), as every
// in-process test does through executor.AssumeASTValidationForTests. Used
// only on a host without a C compiler; a cgo host runs the real layer.
const assumeSQLASTTag = "sage_e2e_assume_sqlast"

// Validation modes of the binary under test, logged with every run.
const (
	validationNative   = "libpg_query"
	validationAssumed  = "assumed"
	validationDegraded = "degraded"
)

// seededObject is one Layer A seed whose gate decisions the dump prints.
type seededObject struct{ check, name string }

// mutatingSeeds are the seeds the autonomous binary must change itself.
var mutatingSeeds = []seededObject{
	{"A01", "pipe_dup_"}, {"A03", "pipe_bloat"}, {"A04", "pipe_stale"},
	{"A05", "pipe_av"}, {"A08", "pipe_invalid_idx"},
}

// pipelineSeeds adds the seeds that must stay advisory or withheld.
var pipelineSeeds = append(append([]seededObject(nil), mutatingSeeds...),
	seededObject{"A02", "pipe_child"}, seededObject{"A06", "pipe_seq"})

type pipelineRunOptions struct {
	tier3Safe bool
	degraded  bool // build without cgo and without the assumption tag
}

// pipelineRun is one real-binary run on the freshly seeded database.
type pipelineRun struct {
	pool       *pgxpool.Pool
	report     *checkReport
	validation string
	out        *syncBuffer // the binary's output
}

// runPipelineBinary seeds the database, builds and starts the binary and
// waits for its first finding. The binary and the slow workload stop in
// cleanup, after any deferred dump.
func runPipelineBinary(t *testing.T, opts pipelineRunOptions) *pipelineRun {
	t.Helper()
	dsn := pipelineURL(t)
	// No Bootstrap here: the binary must bootstrap the sage schema itself
	// (and holds the bootstrap advisory lock while running).
	pool := rawPipelinePool(t)
	run := &pipelineRun{pool: pool, report: &checkReport{}, out: &syncBuffer{}}
	seedBinaryScenarios(t, pool, dsn)

	binPath, validation := buildPipelineBinary(t, opts.degraded)
	run.validation = validation
	t.Logf("binary SQL validation: %s", validation)
	cfgPath := writePipelineConfig(t, dsn, opts.tier3Safe)
	t.Cleanup(startPipelineBinary(t, binPath, cfgPath, dsn, run.out))
	t.Cleanup(runSlowWorkload(t, pool))
	waitForBinaryCycles(t, pool)
	return run
}

// buildPipelineBinary builds the sidecar for Layer A. degraded forces a
// build without cgo, so the gate withholds as a shipped cgo-less binary
// does; otherwise a host without cgo builds with assumeSQLASTTag.
func buildPipelineBinary(t *testing.T, degraded bool) (string, string) {
	t.Helper()
	binName := "pg_sage_sidecar"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(t.TempDir(), binName)
	args := []string{"build", "-o", binPath}
	env := os.Environ()
	validation := validationNative
	switch {
	case degraded:
		env, validation = append(env, "CGO_ENABLED=0"), validationDegraded
	case !hostBuildsWithCgo(t):
		args, validation = append(args, "-tags", assumeSQLASTTag), validationAssumed
	}
	cmd := exec.Command("go", append(args, "./cmd/pg_sage_sidecar/")...)
	cmd.Dir = filepath.Dir(filepath.Dir(mustAbs(t)))
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cannot build the %s binary: %v\n%s", validation, err, out)
	}
	return binPath, validation
}

// hostBuildsWithCgo reports the effective CGO_ENABLED of this host's go
// (0 when no C compiler is on PATH).
func hostBuildsWithCgo(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("go", "env", "CGO_ENABLED").Output()
	if err != nil {
		t.Fatalf("go env CGO_ENABLED: %v", err)
	}
	return strings.TrimSpace(string(out)) == "1"
}

// --- decisions ------------------------------------------------------------------

// decisionRow is one group of sage.decision rows for a seeded object.
type decisionRow struct {
	verdict, reason, riskTier, feature, evidence string
	count                                        int
}

const seedDecisionsSQL = `SELECT verdict, reason, risk_tier, feature, count(*)::int,
	left(max(evidence::text), 160)
  FROM sage.decision
 WHERE strpos(target_objects::text, $1) > 0
    OR strpos(coalesce(evidence->>'proposed_sql', ''), $1) > 0
 GROUP BY 1, 2, 3, 4 ORDER BY 1, 2, 3, 4`

// seedDecisions reads the decision groups of one seeded object.
func seedDecisions(pool *pgxpool.Pool, name string) ([]decisionRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx, seedDecisionsSQL, name)
	if err != nil {
		return nil, fmt.Errorf("read decisions for %s: %w", name, err)
	}
	defer rows.Close()
	var out []decisionRow
	for rows.Next() {
		var r decisionRow
		if err := rows.Scan(&r.verdict, &r.reason, &r.riskTier, &r.feature,
			&r.count, &r.evidence); err != nil {
			return nil, fmt.Errorf("scan decisions for %s: %w", name, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// formatSeedDecisions renders one seeded object's decisions for the dump.
// An object the gate never decided reads "no decision", so a decision
// that could not be written is never mistaken for a silent queue.
func formatSeedDecisions(seed seededObject, rows []decisionRow) string {
	if len(rows) == 0 {
		return fmt.Sprintf("%s %s: no decision", seed.check, seed.name)
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s/%s tier=%s feature=%s x%d evidence=%s",
			r.verdict, reasonOrUnset(r.reason), r.riskTier, r.feature, r.count, r.evidence))
	}
	return fmt.Sprintf("%s %s: %s", seed.check, seed.name, strings.Join(parts, " | "))
}

func reasonOrUnset(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "<no reason>"
	}
	return reason
}

// readErrorVerdict marks a seed whose decisions could not be read.
const readErrorVerdict = "<read error>"

// unexplainedQueues lists the queue verdicts whose reason is empty or not
// one the gate can record (CHECK-A11). A seed whose decisions could not be
// read is listed too: A11 cannot vouch for what it did not see.
func unexplainedQueues(bySeed map[string][]decisionRow) []string {
	var bad []string
	for name, rows := range bySeed {
		for _, r := range rows {
			if r.verdict == readErrorVerdict {
				bad = append(bad, fmt.Sprintf("%s:%s %s", name, readErrorVerdict, r.reason))
				continue
			}
			if r.verdict != string(policy.VerdictQueueApproval) {
				continue
			}
			if !policy.IsLedgerReason(strings.TrimSpace(r.reason)) {
				bad = append(bad, fmt.Sprintf("%s:%s", name, reasonOrUnset(r.reason)))
			}
		}
	}
	return bad
}

// readSeedDecisions reads every seed's decisions; a read error is a
// decision group of its own so the dump and A11 both show it.
func readSeedDecisions(pool *pgxpool.Pool) map[string][]decisionRow {
	bySeed := make(map[string][]decisionRow, len(pipelineSeeds))
	for _, seed := range pipelineSeeds {
		rows, err := seedDecisions(pool, seed.name)
		if err != nil {
			rows = []decisionRow{{verdict: readErrorVerdict, reason: err.Error()}}
		}
		bySeed[seed.name] = rows
	}
	return bySeed
}

// checkDecisionReasons adds CHECK-A11: no seeded object was queued for
// approval without a reason the gate records.
func checkDecisionReasons(t *testing.T, pool *pgxpool.Pool, report *checkReport) {
	t.Helper()
	bad := unexplainedQueues(readSeedDecisions(pool))
	report.add(t, "CHECK-A11", len(bad) == 0,
		fmt.Sprintf("no seeded object queued without a known reason — unexplained=%v", bad))
}

// noteDecisions adds the per-seed decisions and the trust ledger levels to
// the report, read at the end of the run.
func (run *pipelineRun) noteDecisions() {
	run.report.note("binary SQL validation: " + run.validation)
	bySeed := readSeedDecisions(run.pool)
	for _, seed := range pipelineSeeds {
		run.report.note("decision " + formatSeedDecisions(seed, bySeed[seed.name]))
	}
	levels := quickScalar(run.pool, `SELECT coalesce(string_agg(
		family || '/' || action_class || '=L' || level, ', '
		ORDER BY family, action_class), 'none') FROM sage.sre_family_autonomy`)
	run.report.note("trust ledger levels: " + levels)
}

// dump prints the decisions and ledger, then the CHECK lines.
func (run *pipelineRun) dump(t *testing.T, header string) {
	t.Helper()
	run.noteDecisions()
	run.report.dump(t, header)
}

// awaitSeedVerdicts polls until every seed has a decision with verdict
// and reason, then records one CHECK per seed; the dump shows the rest.
func awaitSeedVerdicts(
	t *testing.T, run *pipelineRun, prefix string, seeds []seededObject,
	verdict policy.Verdict, reason policy.Reason,
) {
	t.Helper()
	deadline := time.Now().Add(pipelineBinaryDeadline)
	seen := map[string]bool{}
	for time.Now().Before(deadline) && len(seen) < len(seeds) {
		for _, seed := range seeds {
			if seen[seed.name] {
				continue
			}
			rows, err := seedDecisions(run.pool, seed.name)
			if err == nil && hasVerdict(rows, verdict, reason) {
				seen[seed.name] = true
			}
		}
		if len(seen) < len(seeds) {
			time.Sleep(pipelinePollEvery)
		}
	}
	for _, seed := range seeds {
		run.report.add(t, prefix+"-"+seed.check, seen[seed.name],
			fmt.Sprintf("%s has a %s/%s decision", seed.name, verdict, reason))
	}
}

func hasVerdict(rows []decisionRow, verdict policy.Verdict, reason policy.Reason) bool {
	for _, r := range rows {
		if r.verdict == string(verdict) && r.reason == string(reason) {
			return true
		}
	}
	return false
}

// assertNoActionsOn records that pg_sage ran no successful change on seeds.
func assertNoActionsOn(
	t *testing.T, run *pipelineRun, id string, seeds []seededObject,
) {
	t.Helper()
	var acted []string
	for _, seed := range seeds {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var n int
		err := run.pool.QueryRow(ctx, `SELECT count(*)::int FROM sage.action_log
			WHERE strpos(sql_executed, $1) > 0
			  AND outcome IN ('success','monitoring')`, seed.name).Scan(&n)
		cancel()
		if err != nil || n != 0 {
			acted = append(acted, fmt.Sprintf("%s=%d err=%v", seed.name, n, err))
		}
	}
	run.report.add(t, id, len(acted) == 0,
		fmt.Sprintf("no change ran on the withheld seeds — acted=%v", acted))
}

// A build without parse-tree SQL validation must not mutate unattended
// (Decision 2026-09-27, docs/policy-gate.md): every mutating seed is queued,
// never executed. The classes are grandfathered at L3, so the ledger
// records its handoff reason over the gate's sql_validation_degraded
// queue (internal/executor TestPipelineSeedsQueueWithoutParseTreeValidation
// pins the detail); the binary names the cause at startup.
func TestPipelineGate_DegradedValidationQueuesEveryMutation(t *testing.T) {
	run := runPipelineBinary(t, pipelineRunOptions{tier3Safe: true, degraded: true})
	defer run.dump(t, "Layer A gate — build without parse-tree validation")
	awaitSeedVerdicts(t, run, "CHECK-D01", mutatingSeeds,
		policy.VerdictQueueApproval, policy.ReasonAutonomyHandoff)
	assertNoActionsOn(t, run, "CHECK-D02", mutatingSeeds)
	warned := strings.Contains(run.out.String(), "built without cgo: parse-tree SQL "+
		"validation is unavailable, so unattended changes wait for operator approval")
	run.report.add(t, "CHECK-D03", warned,
		"the binary names the cause at startup (built without cgo)")
	checkDecisionReasons(t, run.pool, run.report)
}

// Invalid input for autonomy: trust.level autonomous with tier3_safe off
// withholds the SAFE vacuum and analyze with the tier reason.
func TestPipelineGate_TierSafeOffWithholdsSafeHygiene(t *testing.T) {
	run := runPipelineBinary(t, pipelineRunOptions{tier3Safe: false})
	defer run.dump(t, "Layer A gate — tier3_safe off")
	safeSeeds := []seededObject{{"A03", "pipe_bloat"}, {"A04", "pipe_stale"}}
	awaitSeedVerdicts(t, run, "CHECK-T01", safeSeeds,
		policy.VerdictBlocked, policy.ReasonTrustRampNotSatisfied)
	assertNoActionsOn(t, run, "CHECK-T02", safeSeeds)
	checkDecisionReasons(t, run.pool, run.report)
}
