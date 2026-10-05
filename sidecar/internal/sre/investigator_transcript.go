package sre

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The investigator's transcript as served by the API and MCP (roadmap
// 2.1): the plan, each tool call with its stored result and digest, the
// citations and the outcome. Redaction is the replay export's (#111):
// default-deny, identifiers become keyed hashes (a fresh key per view)
// unless the operator keeps them, secrets and PII-like literals never
// survive, numbers are kept.

// TranscriptSchema versions the transcript document.
const TranscriptSchema = "pg_sage.sre.investigator_transcript.v1"

// ErrNoTranscript: the investigation did not run the investigator.
var ErrNoTranscript = errors.New("investigation has no investigator transcript")

// TranscriptOptions: KeepIdentifiers is the operator's opt-in; salt fixes
// the hash key (tests; nil: random).
type TranscriptOptions struct {
	KeepIdentifiers bool
	salt            []byte
}

// TranscriptStep is one step with its redacted result.
type TranscriptStep struct {
	InvestigatorStep
	Result *ReplayObservation `json:"result,omitempty"`
}

// TranscriptView is the redacted transcript of one investigation.
type TranscriptView struct {
	Schema          string             `json:"schema"`
	Database        string             `json:"database"`
	InvestigationID UUID               `json:"investigation_id"`
	Plan            string             `json:"plan"`
	Budget          InvestigatorBudget `json:"budget"`
	Protocol        string             `json:"protocol"`
	ModelPlan       string             `json:"model_plan,omitempty"`
	Steps           []TranscriptStep   `json:"steps"`
	Outcome         *ModelConclusion   `json:"outcome,omitempty"`
	Claims          []NarrativeClaim   `json:"claims"`
	DroppedClaims   map[string]int     `json:"dropped_claims,omitempty"`
	Rejected        map[string]int     `json:"rejected,omitempty"`
	ModelCalls      int                `json:"model_calls"`
	Probes          int                `json:"probes"`
	Tokens          int                `json:"tokens"`
	Stop            string             `json:"stop"`
	IdentifiersKept bool               `json:"identifiers_kept"`
	Redaction       []string           `json:"redaction"`
}

// Transcript serves one investigation's redacted transcript.
func (s *Service) Transcript(ctx context.Context, id UUID,
	opts TranscriptOptions) (TranscriptView, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return TranscriptView{}, err
	}
	if err := validateIDs(scope, id); err != nil {
		return TranscriptView{}, err
	}
	inv, err := s.store.Get(ctx, scope, id)
	if err != nil {
		return TranscriptView{}, err
	}
	if inv.Summary.Investigator == nil {
		return TranscriptView{}, fmt.Errorf("%w: %s", ErrNoTranscript, id)
	}
	ev, err := s.store.Evidence(ctx, scope, id)
	if err != nil {
		return TranscriptView{}, err
	}
	key := opts.salt
	if key == nil {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return TranscriptView{}, fmt.Errorf("transcript key: %w", err)
		}
	}
	r := redactor{keep: opts.KeepIdentifiers, key: key}
	return r.transcript(s.name, inv, ev), nil
}

func (r redactor) transcript(db string, inv Investigation, ev []Evidence) TranscriptView {
	run := inv.Summary.Investigator
	v := TranscriptView{Schema: TranscriptSchema, Database: db, InvestigationID: inv.ID,
		Plan: run.Plan, Budget: run.Budget, Protocol: run.Protocol,
		ModelPlan: r.text(run.ModelPlan), Outcome: r.conclusion(inv.Summary.ModelConclusion),
		Claims: []NarrativeClaim{}, DroppedClaims: run.DroppedClaims, Rejected: run.Rejected,
		ModelCalls: run.ModelCalls, Probes: run.Probes, Tokens: run.Tokens, Stop: run.Stop,
		IdentifiersKept: r.keep, Redaction: append([]string{"identifiers_hashed"},
			RedactionRules...)}
	if r.keep {
		v.Redaction[0] = "identifiers_kept"
	}
	byID := map[UUID]Evidence{}
	for _, e := range ev {
		byID[e.ID] = e
	}
	for _, st := range run.Steps {
		step := TranscriptStep{InvestigatorStep: st}
		step.Note = r.text(st.Note)
		step.Args = r.args(st.Args)
		if e, ok := byID[st.EvidenceID]; ok {
			step.Result = r.evidenceResult(e)
		}
		v.Steps = append(v.Steps, step)
	}
	if n := inv.Summary.Narrative; n != nil {
		for _, c := range n.Claims {
			v.Claims = append(v.Claims, NarrativeClaim{Text: r.text(c.Text),
				EvidenceIDs: c.EvidenceIDs})
		}
	}
	return v
}

// conclusion redacts the model's free text in its conclusion.
func (r redactor) conclusion(mc *ModelConclusion) *ModelConclusion {
	if mc == nil {
		return nil
	}
	out := *mc
	out.Reason = r.text(mc.Reason)
	if mc.Cause != nil {
		out.Cause = &UnmodeledCause{Label: r.text(mc.Cause.Label),
			Mechanism: r.text(mc.Cause.Mechanism)}
	}
	return &out
}

// evidenceResult is one stored result, redacted; nil when it does not
// decode.
func (r redactor) evidenceResult(e Evidence) *ReplayObservation {
	var res probes.Result
	dec := json.NewDecoder(bytes.NewReader(e.Payload))
	dec.UseNumber()
	if dec.Decode(&res) != nil {
		return nil
	}
	o := r.result(res)
	return &o
}

// args redacts a step's arguments like row values.
func (r redactor) args(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil {
		return nil
	}
	out, err := json.Marshal(r.row(m))
	if err != nil {
		return nil
	}
	return out
}
