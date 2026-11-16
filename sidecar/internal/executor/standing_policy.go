package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// EnableStandingPolicy installs the durable standing-policy gate and then
// bootstraps the selected profile. The gate is installed first so a bootstrap
// failure leaves the executor fail-closed rather than falling back to legacy
// trust flags.
func (e *Executor) EnableStandingPolicy(
	ctx context.Context, profile string, databaseID *int,
) error {
	return e.EnableStandingPolicyWithStore(ctx, nil, profile, databaseID)
}

// EnableStandingPolicyWithStore reads the standing policy from policyPool
// (the control/meta database the API writes to) while decisions stay in
// the executor's own database. A nil policyPool uses the executor pool
// (standalone). Fleet executors must pass the control pool (G5-B11).
func (e *Executor) EnableStandingPolicyWithStore(
	ctx context.Context, policyPool *pgxpool.Pool,
	profile string, databaseID *int,
) error {
	if e == nil || e.pool == nil {
		return fmt.Errorf("standing policy requires a database pool")
	}
	if policyPool == nil {
		policyPool = e.pool
	}
	policyStore := policy.NewStore(policyPool)
	scope := policy.Scope{DatabaseID: int64Pointer(databaseID)}
	e.policyMu.Lock()
	e.databaseID = databaseID
	e.policyMu.Unlock()
	e.WithPolicyGate(e.newStandingPolicyGate(policyStore, scope))
	e.setExposureMode(func(ctx context.Context) (string, error) {
		current, err := policyStore.Current(ctx, scope)
		if err != nil {
			return "", err
		}
		doc, err := policy.DocumentFromPolicy(current)
		return doc.ExposureMode, err
	})
	cfg, _, _ := e.policySnapshot()
	zone := ""
	if cfg != nil {
		zone = policy.WindowZone(trustWindowExpression(cfg.Trust.MaintenanceWindow))
	}
	current, err := policyStore.BootstrapIn(ctx, scope, profile, zone, "system-bootstrap")
	if err != nil {
		return fmt.Errorf("bootstrap standing policy: %w", err)
	}
	e.warnInvalidStoredPolicy(current)
	e.warnUnzonedPolicyWindows(current)
	return nil
}

// warnUnzonedPolicyWindows logs policy windows that name no time zone:
// they are evaluated on the process clock (UTC in the container), so a
// staffed "weekdays 01:00-05:00" opens at 01:00 UTC (policy-04).
func (e *Executor) warnUnzonedPolicyWindows(current policy.Policy) {
	doc, err := policy.ParseDocument(current.Document)
	if err != nil {
		return // warnInvalidStoredPolicy reports it
	}
	for _, window := range policy.UnzonedWindows(doc) {
		e.logFn("executor", "standing policy v%d window %q names no time zone; it is "+
			"evaluated on the process clock (%s); add a zone such as America/Chicago",
			current.Version, window, time.Local.String())
	}
}

// warnInvalidStoredPolicy logs a stored policy that no longer validates
// (for example an unknown refusal_set token written before tokens were
// checked). The gate fails closed on it, so the operator must learn why
// every action is blocked and ratify a valid version.
func (e *Executor) warnInvalidStoredPolicy(current policy.Policy) {
	if _, err := policy.ParseDocument(current.Document); err != nil {
		e.logFn("executor", "standing policy v%d is invalid, so every action is blocked "+
			"(policy_unavailable) until a valid version is ratified: %v", current.Version, err)
	}
}

// policyReader reads the standing policy in force for a scope
// (policy.Store.Current).
type policyReader interface {
	Current(context.Context, policy.Scope) (policy.Policy, error)
}

// newStandingPolicyGate reads the policy once per decision: the decision
// carries the version and document hash the gate evaluated, and the
// recorder writes those (policy-03), never a second read's.
func (e *Executor) newStandingPolicyGate(
	store policyReader, scope policy.Scope,
) policy.Gate {
	return policy.NewGate(policy.GateConfig{
		Runtime: func(ctx context.Context, request policy.ActionRequest) (policy.RuntimeState, error) {
			return e.standingRuntimeState(ctx, request), nil
		},
		ValidateSQL:  ValidateExecutorSQL,
		Usage:        e.standingUsage,
		Serialize:    e.serializeBudget,
		Autonomy:     e.autonomyLimiter(),
		Facts:        executorFacts{e},
		Verification: e.VerificationWaits(),
		Agents:       executorAgents{e},
		RecordFailed: e.reportRecordFailure,
		Policy: func(ctx context.Context, _ policy.ActionRequest) (policy.Document, error) {
			current, err := store.Current(ctx, scope)
			if err != nil {
				return policy.Document{}, err
			}
			return policy.DocumentFromPolicy(current)
		},
		RecordDecisionDetailed: e.recordStandingDecision,
	})
}

// ledgerInput maps a gate decision onto its ledger row, with the policy
// version and hash the decision carries.
func ledgerInput(
	databaseID *int, request policy.ActionRequest, decision policy.Decision,
) ledger.DecisionInput {
	evidenceID := ledger.NewEvidenceID()
	evidence := cloneCustodianEvidence(request.Evidence)
	stampBudgetEvidence(evidence, request, decision)
	stampVerificationEvidence(evidence, decision)
	autonomyEvidence(evidence, request)
	stampRollbackEvidence(evidence, request, decision)
	if request.Contract == nil {
		evidence["contract"] = "none" // recorded at tier high (policy-01)
	}
	input := ledger.DecisionInput{
		DatabaseID: databaseID, Feature: request.Feature, Intent: ledgerIntent(request),
		Evidence:    evidence,
		ProposedSQL: request.SQL, Verdict: ledgerVerdict(decision.Verdict),
		Reason: string(decision.Reason), RiskTier: string(decision.RiskTier),
		PolicyVersion: int(decision.PolicyVersion), PolicySHA256: decision.PolicySHA256,
		Guardrails: guardrailNames(decision.Guardrails), OffWindowOK: decision.OffWindowOK,
		TargetObjects: request.TargetObjs, EvidenceID: evidenceID,
	}
	budgetBypassReason(&input, decision)
	if request.Principal != nil {
		input.PrincipalID, input.TaskID = request.Principal.ID, request.Principal.TaskID
	}
	input.ArtifactHash = request.ArtifactHash
	if request.Deadline != nil {
		input.DeadlineKind = string(request.Deadline.Kind)
		input.DeadlineHardAt = &request.Deadline.HardAt
	}
	// A withheld verdict repeats every cycle the candidate stays withheld:
	// repeats update one row. An execute verdict backs its own action.
	if input.Verdict != ledger.VerdictExecute {
		nameUnclassified(&input)
		input.Fingerprint = ledger.DecisionFingerprint(input)
	}
	return input
}

// unclassifiedFeature is the ledger feature (and intent) of a withheld
// decision on a statement that maps to no change class, TRUNCATE for one:
// sage.decision requires both, so such a park never reached the ledger
// (policy-01). An execute verdict is never given one: it keeps failing
// validation, closed.
const unclassifiedFeature = "unclassified"

func nameUnclassified(input *ledger.DecisionInput) {
	if strings.TrimSpace(input.Feature) == "" {
		input.Feature = unclassifiedFeature
	}
	if strings.TrimSpace(input.Intent) == "" {
		input.Intent = unclassifiedFeature
	}
}

// ledgerIntent separates operator-approved decisions, which the gate's
// self-initiated usage limits exclude, from autonomous ones.
func ledgerIntent(request policy.ActionRequest) string {
	if request.OperatorApproved {
		return operatorDecisionIntent
	}
	return request.Feature
}

func ledgerVerdict(verdict policy.Verdict) ledger.Verdict {
	if verdict == policy.VerdictPark {
		return ledger.VerdictPark
	}
	return ledger.Verdict(verdict)
}

func guardrailNames(guardrails []policy.Guardrail) []string {
	names := make([]string, 0, len(guardrails))
	for _, guardrail := range guardrails {
		names = append(names, string(guardrail))
	}
	return names
}

func int64Pointer(value *int) *int64 {
	if value == nil {
		return nil
	}
	result := int64(*value)
	return &result
}

// EnableStandingPolicyDocument installs the standing gate over the
// executor's live runtime state with an in-memory policy document instead
// of the durable policy store. Decisions are recorded in the ledger when
// the executor has a pool. It serves embedders and tests without a policy
// table; production executors use EnableStandingPolicyWithStore.
func (e *Executor) EnableStandingPolicyDocument(doc policy.Document, now func() time.Time) {
	doc = inMemoryDocument(doc)
	e.setExposureMode(func(context.Context) (string, error) { return doc.ExposureMode, nil })
	config := policy.GateConfig{
		Runtime: func(ctx context.Context, req policy.ActionRequest) (policy.RuntimeState, error) {
			state := e.standingRuntimeState(ctx, req)
			if now != nil {
				cfg, _, _ := e.policySnapshot()
				state.InConfiguredWindow = inMaintenanceWindowForPolicy(cfg, now())
			}
			return state, nil
		},
		ValidateSQL: ValidateExecutorSQL,
		Policy: func(context.Context, policy.ActionRequest) (policy.Document, error) {
			return doc, nil
		},
		Now:      now,
		Autonomy: e.autonomyLimiter(),
		Facts:    executorFacts{e},
		Agents:   executorAgents{e},
	}
	if e.pool != nil {
		config.Usage = e.standingUsage
		config.Serialize = e.serializeBudget
		config.Verification = e.VerificationWaits()
		config.RecordDecisionDetailed = e.recordStandingDecision
		config.RecordFailed = e.reportRecordFailure
	}
	e.WithPolicyGate(policy.NewGate(config))
}

// inMemoryDocument names an in-memory document as version 1 with the
// SHA-256 of its canonical encoding, so its decisions name the policy they
// were made under as stored ones do. A document that does not encode (it
// fails validation, and the gate blocks every action on it) has no hash.
func inMemoryDocument(doc policy.Document) policy.Document {
	if doc.Version == 0 {
		doc.Version = 1
	}
	if doc.SHA256 == "" {
		if raw, err := policy.MarshalDocument(doc); err == nil {
			sum := sha256.Sum256(raw)
			doc.SHA256 = hex.EncodeToString(sum[:])
		}
	}
	return doc
}

// WithEmergencyStopCheck replaces how the executor reads the emergency-stop
// flag (default: sage.config in the executor's database).
func (e *Executor) WithEmergencyStopCheck(check func(context.Context) bool) {
	e.policyMu.Lock()
	defer e.policyMu.Unlock()
	e.emergencyStopFn = check
}

// setExposureMode installs the reader of the standing document's exposure
// mode (fleetpol-02b), with the gate it belongs to.
func (e *Executor) setExposureMode(read func(context.Context) (string, error)) {
	e.policyMu.Lock()
	e.exposureMode = read
	e.policyMu.Unlock()
}
