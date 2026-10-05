package sre

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/sre/causal"
)

// Persisted output of the tool-calling investigator (roadmap 2.1): the
// model's conclusion with the authority it got, and the transcript of
// the run (plan, budget, every tool call with its evidence and digest,
// refusals, dropped claims, why it stopped). Both live in the summary
// beside the deterministic diagnosis, are labeled wherever shown, and
// are validated again before any store I/O.

// Labels.
const (
	ModelConclusionLabel = "model conclusion"
	InvestigatorRunLabel = "model investigator transcript"
)

// Model conclusion outcomes, normalized against the graph.
const (
	ModelAgreed       = "agreed"
	ModelConcluded    = "concluded"
	ModelContested    = "contested"
	ModelUnmodeled    = "unmodeled"
	ModelInconclusive = "inconclusive"
)

// Transcript bounds.
const (
	MaxInvestigatorSteps  = 64
	maxModelPlanRunes     = 500
	maxStepArgsBytes      = 512
	maxStepNoteRunes      = 300
	maxCauseLabelRunes    = 200
	maxCauseMechanismRune = 600
)

// UnmodeledCause is a cause the causal graph has no node for.
type UnmodeledCause struct {
	Label     string `json:"label"`
	Mechanism string `json:"mechanism"`
}

// ModelConclusion is what the investigator concluded and the authority
// it got: adopted only through the family's earned root authority.
type ModelConclusion struct {
	Label     string          `json:"label"`
	Outcome   string          `json:"outcome"`
	Root      string          `json:"root,omitempty"`
	Cause     *UnmodeledCause `json:"cause,omitempty"`
	GraphRoot string          `json:"graph_root,omitempty"`
	Authority string          `json:"authority"`
	Reason    string          `json:"reason"`
}

// InvestigatorBudget is the plan's budget as run.
type InvestigatorBudget struct {
	MaxSteps  int   `json:"max_steps"`
	MaxProbes int   `json:"max_probes"`
	WallMS    int64 `json:"wall_ms"`
	MaxTokens int64 `json:"max_tokens"`
}

// InvestigatorStep is one transcript event: a tool call (run or
// refused), a refused reply, the protocol switch or the final answer.
type InvestigatorStep struct {
	Seq        int             `json:"seq"`
	Call       int             `json:"call"`
	Tool       string          `json:"tool,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Status     string          `json:"status"`
	EvidenceID UUID            `json:"evidence_id,omitempty"`
	Digest     string          `json:"digest,omitempty"`
	Cost       int             `json:"cost,omitempty"`
	ElapsedMS  int64           `json:"elapsed_ms"`
	Note       string          `json:"note,omitempty"`
}

// InvestigatorRun is the stored transcript of one investigator run.
type InvestigatorRun struct {
	Label         string             `json:"label"`
	Plan          string             `json:"plan"`
	Budget        InvestigatorBudget `json:"budget"`
	Protocol      string             `json:"protocol"`
	ModelPlan     string             `json:"model_plan,omitempty"`
	Steps         []InvestigatorStep `json:"steps"`
	ModelCalls    int                `json:"model_calls"`
	ToolCalls     int                `json:"tool_calls"`
	Probes        int                `json:"probes"`
	Tokens        int                `json:"tokens"`
	Rejected      map[string]int     `json:"rejected,omitempty"`
	DroppedClaims map[string]int     `json:"dropped_claims,omitempty"`
	Stop          string             `json:"stop"`
	StopDetail    string             `json:"stop_detail,omitempty"`
}

var conclusionOutcomes = map[string]bool{ModelAgreed: true, ModelConcluded: true,
	ModelContested: true, ModelUnmodeled: true, ModelInconclusive: true}

func (m *ModelConclusion) validate(hs []HypothesisRecord, root string, concluded bool) error {
	if m == nil {
		return nil
	}
	if err := m.validateShape(); err != nil {
		return err
	}
	if m.Root != "" {
		if _, ok := causal.NodeByID(causal.NodeID(m.Root)); !ok {
			return fmt.Errorf("%w: model conclusion names %q, not a graph node",
				ErrInvalidRequest, m.Root)
		}
	}
	adopted := m.Authority == ContestAdopted
	switch {
	case adopted && (!concluded || root != m.Root):
		return fmt.Errorf("%w: an adopted model conclusion must be the root %q",
			ErrInvalidRequest, root)
	case !adopted && concluded && root == m.Root &&
		(m.Outcome == ModelContested || m.Outcome == ModelConcluded):
		return fmt.Errorf("%w: an advisory %s conclusion cannot be the root",
			ErrInvalidRequest, m.Outcome)
	}
	return nil
}

func (m *ModelConclusion) validateShape() error {
	reason := strings.TrimSpace(m.Reason)
	unmodeled := m.Outcome == ModelUnmodeled
	switch {
	case m.Label != ModelConclusionLabel:
		return fmt.Errorf("%w: model conclusion must carry its label", ErrInvalidRequest)
	case !conclusionOutcomes[m.Outcome]:
		return fmt.Errorf("%w: model conclusion outcome %q", ErrInvalidRequest, m.Outcome)
	case m.Authority != ContestAdvisory && m.Authority != ContestAdopted:
		return fmt.Errorf("%w: model conclusion authority %q", ErrInvalidRequest,
			m.Authority)
	case reason == "" || checkText("reason", m.Reason, true, maxContestReasonRunes) != nil:
		return fmt.Errorf("%w: model conclusion reason", ErrInvalidRequest)
	case unmodeled != (m.Cause != nil):
		return fmt.Errorf("%w: only an unmodeled conclusion has a cause", ErrInvalidRequest)
	case unmodeled && (m.Root != "" || m.Authority == ContestAdopted):
		return fmt.Errorf("%w: an unmodeled cause has no graph node and stays advisory",
			ErrInvalidRequest)
	}
	if unmodeled {
		return m.Cause.validate()
	}
	return nil
}

func (c *UnmodeledCause) validate() error {
	if err := checkText("cause label", strings.TrimSpace(c.Label), true,
		maxCauseLabelRunes); err != nil {
		return err
	}
	return checkText("cause mechanism", strings.TrimSpace(c.Mechanism), true,
		maxCauseMechanismRune)
}

func (r *InvestigatorRun) validate() error {
	if r == nil {
		return nil
	}
	switch {
	case r.Label != InvestigatorRunLabel:
		return fmt.Errorf("%w: investigator transcript must carry its label",
			ErrInvalidRequest)
	case r.Plan != PlanNarrow && r.Plan != PlanBroad:
		return fmt.Errorf("%w: investigator plan %q", ErrInvalidRequest, r.Plan)
	case len(r.Steps) > MaxInvestigatorSteps:
		return fmt.Errorf("%w: %d transcript steps over %d", ErrInvalidRequest,
			len(r.Steps), MaxInvestigatorSteps)
	case checkText("stop", r.Stop, true, 64) != nil:
		return fmt.Errorf("%w: investigator stop reason", ErrInvalidRequest)
	case utf8.RuneCountInString(r.ModelPlan) > maxModelPlanRunes:
		return fmt.Errorf("%w: model plan too long", ErrInvalidRequest)
	}
	for i, s := range r.Steps {
		if err := s.validate(); err != nil {
			return fmt.Errorf("transcript step %d: %w", i+1, err)
		}
	}
	return nil
}

func (s InvestigatorStep) validate() error {
	switch {
	case s.Cost < 0 || len(s.Args) > maxStepArgsBytes:
		return fmt.Errorf("%w: step cost or arguments", ErrInvalidRequest)
	case len(s.Args) > 0 && !json.Valid(s.Args):
		return fmt.Errorf("%w: step arguments are not JSON", ErrInvalidRequest)
	case checkText("note", s.Note, false, maxStepNoteRunes) != nil,
		checkText("tool", s.Tool, false, 64) != nil,
		checkText("status", s.Status, true, 64) != nil:
		return fmt.Errorf("%w: step text", ErrInvalidRequest)
	}
	if s.EvidenceID != "" {
		if _, err := ParseUUID(string(s.EvidenceID)); err != nil {
			return err
		}
	}
	if s.Digest != "" {
		if raw, err := hex.DecodeString(s.Digest); err != nil || len(raw) != 32 {
			return fmt.Errorf("%w: step digest", ErrInvalidRequest)
		}
	}
	return nil
}

// evidenceRefs lists the evidence ids the transcript cites.
func (r *InvestigatorRun) evidenceRefs() []UUID {
	if r == nil {
		return nil
	}
	var out []UUID
	for _, s := range r.Steps {
		if s.EvidenceID != "" {
			out = append(out, s.EvidenceID)
		}
	}
	return out
}
