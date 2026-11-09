package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	ErrRepositoryUnavailable = errors.New("ledger repository unavailable")
	ErrEvidenceConflict      = errors.New("evidence ID conflict")
	// ErrInvalidRiskTier rejects a tier sage.decision's CHECK would refuse,
	// before any SQL runs (policy-01).
	ErrInvalidRiskTier = errors.New("invalid decision risk_tier")
)

type Repository interface {
	InsertDecision(context.Context, DecisionInput) (int64, error)
	FindAuditViolations(context.Context) ([]AuditViolation, error)
}

type Service struct {
	repository Repository
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository}
}

func (s *Service) RecordDecision(
	ctx context.Context, input DecisionInput,
) (Decision, error) {
	if s == nil || s.repository == nil {
		return Decision{}, ErrRepositoryUnavailable
	}
	if err := validateDecision(input); err != nil {
		return Decision{}, err
	}
	if input.EvidenceID == "" {
		input.EvidenceID = NewEvidenceID()
	}
	if upserter, ok := s.repository.(FingerprintRepository); ok && input.Fingerprint != "" {
		id, evidenceID, err := upserter.UpsertDecision(ctx, input)
		if err != nil {
			return Decision{}, fmt.Errorf("upsert decision: %w", err)
		}
		input.EvidenceID = evidenceID
		return Decision{DecisionInput: input, ID: id}, nil
	}
	id, err := s.repository.InsertDecision(ctx, input)
	if err != nil {
		return Decision{}, fmt.Errorf("insert decision: %w", err)
	}
	return Decision{DecisionInput: input, ID: id}, nil
}

func (s *Service) SelfAudit(ctx context.Context) (AuditResult, error) {
	if s == nil || s.repository == nil {
		return AuditResult{}, ErrRepositoryUnavailable
	}
	violations, err := s.repository.FindAuditViolations(ctx)
	if err != nil {
		return AuditResult{}, fmt.Errorf("find audit violations: %w", err)
	}
	if violations == nil {
		violations = []AuditViolation{}
	}
	return AuditResult{
		OK: len(violations) == 0, Count: len(violations),
		Violations: violations,
	}, nil
}

func NewEvidenceID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("generate evidence ID: %v", err))
	}
	return "ev_" + hex.EncodeToString(raw[:])
}

func validateDecision(input DecisionInput) error {
	checks := []struct {
		missing bool
		name    string
	}{
		{strings.TrimSpace(input.Feature) == "", "feature"},
		{strings.TrimSpace(input.Intent) == "", "intent"},
		{strings.TrimSpace(input.Reason) == "", "reason"},
	}
	for _, check := range checks {
		if check.missing {
			return fmt.Errorf("decision %s is required", check.name)
		}
	}
	if err := validatePolicyProvenance(input); err != nil {
		return err
	}
	if !validVerdict(input.Verdict) {
		return fmt.Errorf("decision verdict %q is invalid", input.Verdict)
	}
	if !validRiskTier(input.RiskTier) {
		return fmt.Errorf("%w %q: want read_only, safe, moderate or high",
			ErrInvalidRiskTier, input.RiskTier)
	}
	if input.DeadlineKind != "" && input.DeadlineHardAt == nil {
		return errors.New("decision deadline requires deadline_hard_at")
	}
	return nil
}

// validatePolicyProvenance mirrors sage.decision's CHECK on the hash. A
// decision made before any policy read has version 0 and no hash; a hash
// always names the version it is the hash of.
func validatePolicyProvenance(input DecisionInput) error {
	switch {
	case input.PolicyVersion < 0:
		return fmt.Errorf("decision policy_version %d is negative", input.PolicyVersion)
	case input.PolicySHA256 == "":
		return nil
	case !policyHashPattern.MatchString(input.PolicySHA256):
		return fmt.Errorf("decision policy_doc_sha256 %q is not 64 lower-case hex digits",
			input.PolicySHA256)
	case input.PolicyVersion == 0:
		return errors.New("decision policy_doc_sha256 requires the policy_version it hashes")
	}
	return nil
}

var policyHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validVerdict(verdict Verdict) bool {
	switch verdict {
	case VerdictExecute, VerdictQueueApproval, VerdictPark,
		VerdictBlocked, VerdictObserveOnly:
		return true
	default:
		return false
	}
}

// validRiskTier mirrors sage.decision's CHECK on risk_tier.
func validRiskTier(tier string) bool {
	switch tier {
	case "read_only", "safe", "moderate", "high":
		return true
	default:
		return false
	}
}
