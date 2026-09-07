package ledger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrRepositoryUnavailable = errors.New("ledger repository unavailable")
	ErrEvidenceConflict      = errors.New("evidence ID conflict")
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
		{input.PolicyVersion <= 0, "policy_version"},
	}
	for _, check := range checks {
		if check.missing {
			return fmt.Errorf("decision %s is required", check.name)
		}
	}
	if !validVerdict(input.Verdict) {
		return fmt.Errorf("decision verdict %q is invalid", input.Verdict)
	}
	if input.DeadlineKind != "" && input.DeadlineHardAt == nil {
		return errors.New("decision deadline requires deadline_hard_at")
	}
	return nil
}

func validVerdict(verdict Verdict) bool {
	switch verdict {
	case VerdictExecute, VerdictQueueApproval, VerdictPark,
		VerdictBlocked, VerdictObserveOnly:
		return true
	default:
		return false
	}
}
