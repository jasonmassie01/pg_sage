package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

type admissionFixture struct {
	pool     *pgxpool.Pool
	ctx      context.Context
	exec     *Executor
	database string
	table    string
	logs     *[]string
	mu       *sync.Mutex
}

func newAdmissionFixture(t *testing.T, admission verify.Admission) admissionFixture {
	t.Helper()
	pool, ctx := requireDB(t)
	suffix := time.Now().UnixNano()
	fixture := admissionFixture{
		pool: pool, ctx: ctx, database: fmt.Sprintf("d6_db_%d", suffix),
		table: fmt.Sprintf("d6_orders_%d", suffix), logs: &[]string{}, mu: &sync.Mutex{},
	}
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+fixture.table+
		" (id bigint, customer_id bigint)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { fixture.cleanup() })
	logFn := func(_ string, format string, args ...any) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		*fixture.logs = append(*fixture.logs, fmt.Sprintf(format, args...))
	}
	fixture.exec = New(pool, config.DefaultConfig(), zeroTime(), logFn)
	fixture.exec.WithDatabaseName(fixture.database)
	fixture.exec.indexVerification = newVerifiedIndexLifecycle(
		&fakeIndexVerifier{admission: admission}, &fakeVerifiedIndexActions{},
	)
	return fixture
}

func (f admissionFixture) cleanup() {
	ctx := context.Background()
	_, _ = f.pool.Exec(ctx, "DELETE FROM sage.admission_withheld WHERE database_name=$1",
		f.database)
	_, _ = f.pool.Exec(ctx, "DROP TABLE IF EXISTS public."+f.table)
}

func (f admissionFixture) finding() analyzer.Finding {
	index := "idx_" + f.table
	return analyzer.Finding{
		Category: "missing_index",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY " + index +
			" ON public." + f.table + " (customer_id)",
		RollbackSQL:      "DROP INDEX CONCURRENTLY IF EXISTS public." + index,
		ObjectIdentifier: "public." + f.table,
		Detail:           map[string]any{"queryids": []int64{7, 9}},
		Title:            "missing index",
	}
}

func (f admissionFixture) countLogs(substring string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, line := range *f.logs {
		if strings.Contains(line, substring) {
			count++
		}
	}
	return count
}

func learningAdmission() verify.Admission {
	return verify.Admission{
		Reason: verify.ReasonLearningBaseline, Mode: verify.EvidenceLearnedBaseline,
		Detail:   "learning IO baseline: 3.0/7 days",
		Evidence: map[string]any{"evidence_mode": verify.EvidenceLearnedBaseline},
	}
}

func withheldRows(t *testing.T, f admissionFixture) (rows, occurrences int, reason string) {
	t.Helper()
	err := f.pool.QueryRow(f.ctx, `SELECT count(*), COALESCE(sum(occurrences),0),
		COALESCE(max(reason),'') FROM sage.admission_withheld WHERE database_name=$1`,
		f.database).Scan(&rows, &occurrences, &reason)
	if err != nil {
		t.Fatalf("read withheld admissions: %v", err)
	}
	return rows, occurrences, reason
}

func TestWithheldCreateIndexLogsOnce(t *testing.T) {
	f := newAdmissionFixture(t, learningAdmission())
	finding := f.finding()
	decisionID := recordCustodianDecision(t, f.ctx, f.pool, "index", f.table)
	for range 3 {
		f.exec.executeFinding(f.ctx, finding, 0, ActionPolicyDecision{DecisionID: decisionID})
	}
	var actions int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sage.action_log
		WHERE sql_executed=$1`, finding.RecommendedSQL).Scan(&actions); err != nil {
		t.Fatalf("count actions: %v", err)
	}
	if actions != 0 {
		t.Fatalf("withheld admission wrote %d failed action rows, want 0", actions)
	}
	rows, occurrences, reason := withheldRows(t, f)
	if rows != 1 || occurrences != 3 || reason != verify.ReasonLearningBaseline {
		t.Fatalf("withheld rows=%d occurrences=%d reason=%q, want 1/3/learning",
			rows, occurrences, reason)
	}
	if got := f.countLogs("withheld"); got != 1 {
		t.Fatalf("withheld log lines = %d, want 1", got)
	}
	var mode string
	if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(evidence->'load_admission'->>'mode','')
		FROM sage.decision WHERE id=$1`, decisionID).Scan(&mode); err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if mode != verify.EvidenceLearnedBaseline {
		t.Fatalf("decision evidence mode = %q, want learned_baseline", mode)
	}
}

func TestWithheldAdmissionNewReasonRecordsSeparately(t *testing.T) {
	f := newAdmissionFixture(t, learningAdmission())
	finding := f.finding()
	f.exec.executeFinding(f.ctx, finding, 0, ActionPolicyDecision{})
	f.exec.indexVerification = newVerifiedIndexLifecycle(&fakeIndexVerifier{
		admission: verify.Admission{Reason: verify.ReasonDataIOAboveBaseline,
			Mode: verify.EvidenceLearnedBaseline},
	}, &fakeVerifiedIndexActions{})
	f.exec.executeFinding(f.ctx, finding, 0, ActionPolicyDecision{})
	rows, occurrences, _ := withheldRows(t, f)
	if rows != 2 || occurrences != 2 {
		t.Fatalf("rows=%d occurrences=%d, want one row per reason", rows, occurrences)
	}
}

// staticVerifier is a concurrency-safe verifier with a fixed admission.
type staticVerifier struct {
	fakeIndexVerifier
	admission verify.Admission
}

func (s *staticVerifier) OKToApplyNow(context.Context) (verify.Admission, error) {
	return s.admission, nil
}

func TestConcurrentWithheldAdmissionsRecordOneRow(t *testing.T) {
	f := newAdmissionFixture(t, learningAdmission())
	f.exec.indexVerification = newVerifiedIndexLifecycle(
		&staticVerifier{admission: learningAdmission()}, &fakeVerifiedIndexActions{},
	)
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := f.exec.admitIndexBuild(f.ctx, "finding:42", 0)
			if !errors.Is(err, ErrVerificationUnavailable) {
				t.Errorf("admission error = %v", err)
			}
		}()
	}
	workers.Wait()
	rows, occurrences, _ := withheldRows(t, f)
	if rows != 1 || occurrences != 8 {
		t.Fatalf("concurrent withholds rows=%d occurrences=%d, want 1/8", rows, occurrences)
	}
}

func TestAdmittedDecisionRecordsCapacityAttestation(t *testing.T) {
	f := newAdmissionFixture(t, verify.Admission{})
	f.exec.cfg = declaredConfig(1000, 100)
	f.exec.WithHostCPUReader(testHostCPU{cpu: 20})
	f.exec.WithIOEvidence(fakeIOEvidence{evidence: ioEvidenceAt(100, 10, verify.IOBaseline{})})
	engine := admissionEngine(t, f.exec)
	f.exec.indexVerification = newVerifiedIndexLifecycle(
		&postgresIndexVerifier{engine: engine, exec: f.exec}, &fakeVerifiedIndexActions{},
	)
	decisionID := recordCustodianDecision(t, f.ctx, f.pool, "index", f.table)
	if err := f.exec.admitIndexBuild(f.ctx, "finding:7", decisionID); err != nil {
		t.Fatalf("admitIndexBuild: %v", err)
	}
	var mode, readWrite, wal, dataPct string
	if err := f.pool.QueryRow(f.ctx, `SELECT
		COALESCE(evidence->'load_admission'->>'mode',''),
		COALESCE(evidence->'load_admission'->'evidence'->>'declared_read_write_mbps',''),
		COALESCE(evidence->'load_admission'->'evidence'->>'declared_wal_mbps',''),
		COALESCE(evidence->'load_admission'->'evidence'->>'data_io_pct','')
		FROM sage.decision WHERE id=$1`, decisionID).
		Scan(&mode, &readWrite, &wal, &dataPct); err != nil {
		t.Fatalf("read decision evidence: %v", err)
	}
	if mode != verify.EvidenceDeclaredCapacity || readWrite != "1000" || wal != "100" ||
		dataPct != "10" {
		t.Fatalf("decision evidence mode=%q rw=%q wal=%q data=%q", mode, readWrite, wal, dataPct)
	}
	if rows, _, _ := withheldRows(t, f); rows != 0 {
		t.Fatalf("admitted build recorded %d withheld rows", rows)
	}
}

func TestCustodianIndexWithheldRecordsOnce(t *testing.T) {
	f := newAdmissionFixture(t, learningAdmission())
	f.exec.WithPolicyGate(&custodianGateCapture{verdict: policy.Decision{
		Verdict: policy.VerdictExecute, RiskTier: policy.RiskModerate,
	}})
	proposal := CustodianProposal{
		Feature: "fk_index", SQL: f.finding().RecommendedSQL,
		TargetObjects: []string{"public." + f.table},
	}
	for range 2 {
		err := f.exec.SubmitVerifiedIndexProposal(
			f.ctx, proposal, f.finding().RollbackSQL, []int64{7},
		)
		if !errors.Is(err, ErrVerificationUnavailable) {
			t.Fatalf("custodian admission error = %v", err)
		}
	}
	rows, occurrences, reason := withheldRows(t, f)
	if rows != 1 || occurrences != 2 || reason != verify.ReasonLearningBaseline {
		t.Fatalf("custodian withheld rows=%d occurrences=%d reason=%q", rows, occurrences, reason)
	}
}

func TestIndexAdmissionStatusReportsProgressAndWithheld(t *testing.T) {
	admission := learningAdmission()
	admission.Evidence = map[string]any{
		"evidence_mode":          verify.EvidenceLearnedBaseline,
		"baseline_observed_days": 3.0, "baseline_samples": 4320,
	}
	f := newAdmissionFixture(t, admission)
	if err := f.exec.admitIndexBuild(f.ctx, "finding:1", 0); err == nil {
		t.Fatal("learning baseline admitted")
	}
	status := f.exec.IndexAdmissionStatus(f.ctx)
	if status.Reason != verify.ReasonLearningBaseline || status.Detail != admission.Detail ||
		status.BaselineObservedDays != 3 || status.BaselineRequiredDays != 7 ||
		status.BaselineSamples != 4320 {
		t.Fatalf("status = %+v", status)
	}
	if status.WithheldFindings != 1 || status.LastWithheldAt == nil {
		t.Fatalf("withheld summary = %d %v", status.WithheldFindings, status.LastWithheldAt)
	}
	// Reading status never records a withheld admission itself.
	if _, occurrences, _ := withheldRows(t, f); occurrences != 1 {
		t.Fatalf("status check recorded a withhold: occurrences=%d", occurrences)
	}
}
