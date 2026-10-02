package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// IndexAdmissionStatus is the operator-facing load-admission state of one
// database's autonomous index builds.
type IndexAdmissionStatus struct {
	Database             string         `json:"database"`
	OK                   bool           `json:"ok"`
	Reason               string         `json:"reason"`
	Mode                 string         `json:"mode"`
	Detail               string         `json:"detail"`
	BaselineRequiredDays float64        `json:"baseline_required_days"`
	BaselineObservedDays float64        `json:"baseline_observed_days"`
	BaselineSamples      int            `json:"baseline_samples"`
	MissingEvidence      []string       `json:"missing_evidence"`
	Evidence             map[string]any `json:"evidence,omitempty"`
	WithheldFindings     int            `json:"withheld_findings"`
	LastWithheldAt       *time.Time     `json:"last_withheld_at,omitempty"`
	CheckedAt            time.Time      `json:"checked_at"`
}

// AdmissionWithheldError reports that load admission withheld an index
// build. It wraps ErrVerificationUnavailable and the underlying cause.
type AdmissionWithheldError struct {
	Admission verify.Admission
	Cause     error
}

func (e *AdmissionWithheldError) Error() string {
	message := "load admission withheld: " + e.Admission.Reason
	if e.Admission.Detail != "" {
		message += ": " + e.Admission.Detail
	}
	return ErrVerificationUnavailable.Error() + ": " + message
}

func (e *AdmissionWithheldError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrVerificationUnavailable}
	}
	return []error{ErrVerificationUnavailable, e.Cause}
}

// isAdmissionWithheld reports a load-admission withhold, which is recorded
// once per finding and reason instead of as a failed action every cycle.
func isAdmissionWithheld(err error) bool {
	var withheld *AdmissionWithheldError
	return errors.As(err, &withheld)
}

// admissionFindingKey identifies a finding for once-per-reason recording.
// Custodian proposals have no finding row, so their SQL identifies them.
func admissionFindingKey(findingID int64, sql string) string {
	if findingID > 0 {
		return "finding:" + strconv.FormatInt(findingID, 10)
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(sql)))
	return "sql:" + hex.EncodeToString(sum[:8])
}

// admitIndexBuild runs load admission for one autonomous index build. The
// verdict and its evidence are recorded in the authorizing decision; a
// withheld verdict is recorded once per finding and reason.
func (e *Executor) admitIndexBuild(
	ctx context.Context, findingKey string, decisionID int64,
) error {
	admission, err := e.indexVerification.Admit(ctx)
	var withheld *AdmissionWithheldError
	if err != nil && !errors.As(err, &withheld) {
		return err
	}
	e.recordAdmissionDecision(ctx, decisionID, admission)
	if withheld == nil {
		return nil
	}
	if e.recordWithheldAdmission(ctx, findingKey, decisionID, admission) {
		e.logFn("executor", "withheld autonomous index build %s: %v", findingKey, err)
	}
	return err
}

func (e *Executor) recordAdmissionDecision(
	ctx context.Context, decisionID int64, admission verify.Admission,
) {
	if e.pool == nil || decisionID <= 0 {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"ok": admission.OK, "reason": admission.Reason, "mode": admission.Mode,
		"detail": admission.Detail, "evidence": admission.Evidence,
	})
	if err == nil {
		_, err = e.pool.Exec(ctx, `UPDATE sage.decision
			SET evidence = evidence || jsonb_build_object('load_admission', $1::jsonb)
			WHERE id = $2`, string(payload), decisionID)
	}
	if err != nil {
		e.logFn("executor", "record load admission for decision %d: %v", decisionID, err)
	}
}

// recordWithheldAdmission upserts the withheld record and reports whether
// it is new, so the log line is written once per finding and reason.
func (e *Executor) recordWithheldAdmission(
	ctx context.Context, findingKey string, decisionID int64, admission verify.Admission,
) bool {
	if e.pool == nil {
		return true
	}
	evidence, err := json.Marshal(admission.Evidence)
	if err != nil {
		evidence = []byte("{}")
	}
	var inserted bool
	err = e.pool.QueryRow(ctx, `INSERT INTO sage.admission_withheld
		(database_name, finding_key, reason, mode, detail, evidence, decision_id)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, NULLIF($7, 0))
		ON CONFLICT (database_name, finding_key, reason) DO UPDATE SET
			last_seen_at = now(), occurrences = sage.admission_withheld.occurrences + 1,
			mode = EXCLUDED.mode, detail = EXCLUDED.detail, evidence = EXCLUDED.evidence,
			decision_id = COALESCE(EXCLUDED.decision_id, sage.admission_withheld.decision_id)
		RETURNING (xmax = 0)`,
		e.databaseName, findingKey, admission.Reason, admission.Mode,
		admission.Detail, string(evidence), decisionID).Scan(&inserted)
	if err != nil {
		e.logFn("executor", "record withheld admission %s: %v", findingKey, err)
		return true
	}
	return inserted
}

// admissionWindowOpen evaluates the maintenance window exactly as the
// standing gate does for an unattended moderate index build.
func (e *Executor) admissionWindowOpen(ctx context.Context) bool {
	reporter, ok := e.StandingPolicyGate().(policy.WindowReporter)
	if !ok {
		return false
	}
	open, err := reporter.MaintenanceWindowOpen(ctx, policy.ActionRequest{
		Feature: string(policy.ChangeIndex),
		Contract: &policy.ActionContract{
			ActionType: "create_index_concurrently", RiskTier: policy.RiskModerate,
		},
	})
	return err == nil && open
}

// IndexAdmissionStatus evaluates admission now without recording anything.
func (e *Executor) IndexAdmissionStatus(ctx context.Context) IndexAdmissionStatus {
	cfg, _, _ := e.policySnapshot()
	status := IndexAdmissionStatus{
		Database: e.databaseName, CheckedAt: time.Now().UTC(),
		MissingEvidence: e.missingAdmissionEvidence(cfg),
	}
	if cfg != nil {
		status.BaselineRequiredDays = cfg.Verify.EffectiveIOBaselineDays()
	}
	admission, err := e.indexVerification.Admit(ctx)
	if errors.Is(err, ErrVerificationUnavailable) && admission.Reason == "" {
		admission = verify.Admission{
			Reason: verify.ReasonLoadUnavailable, Mode: verify.EvidenceUnavailable,
			Detail: "index verification is not configured for this database",
		}
	}
	status.OK, status.Reason, status.Mode = admission.OK, admission.Reason, admission.Mode
	status.Detail, status.Evidence = admission.Detail, admission.Evidence
	status.BaselineObservedDays = evidenceFloat(admission.Evidence, "baseline_observed_days")
	status.BaselineSamples = int(evidenceFloat(admission.Evidence, "baseline_samples"))
	e.withheldSummary(ctx, &status)
	return status
}

// missingAdmissionEvidence names the inputs this database cannot supply.
func (e *Executor) missingAdmissionEvidence(cfg *config.Config) []string {
	e.policyMu.RLock()
	cpuReader, ioReader := e.hostCPU, e.ioEvidence
	e.policyMu.RUnlock()
	missing := []string{}
	if ioReader == nil {
		missing = append(missing, "pg_io_rate")
	}
	if cpuReader == nil {
		missing = append(missing, "host_cpu")
	}
	if cfg == nil || (cfg.Verify.IOCapacity == nil && cfg.Verify.EffectiveIOBaselineDays() <= 0) {
		missing = append(missing, "io_capacity_or_baseline")
	}
	return missing
}

func evidenceFloat(evidence map[string]any, key string) float64 {
	switch value := evidence[key].(type) {
	case float64:
		return value
	case int:
		return float64(value)
	default:
		return 0
	}
}

func (e *Executor) withheldSummary(ctx context.Context, status *IndexAdmissionStatus) {
	if e.pool == nil {
		return
	}
	err := e.pool.QueryRow(ctx, `SELECT count(*), max(last_seen_at)
		FROM sage.admission_withheld WHERE database_name = $1`, e.databaseName).
		Scan(&status.WithheldFindings, &status.LastWithheldAt)
	if err != nil {
		e.logFn("executor", "read withheld admissions: %v", err)
	}
}
