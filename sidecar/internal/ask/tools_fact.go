package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/facts"
)

// propose_fact (owner decision 2026-10-04): a caller who may propose can
// have Ask Sage propose one binding fact per question, citing evidence
// this run read. The fact stays proposed until a person confirms it; a
// fact already decided is never changed (the store keeps a rejected or
// confirmed fact as it is), and Ask Sage has no confirm path at all.

const maxFactEvidence = 6

func (ss *session) proposeFactTool() agentloop.Tool {
	return tool("propose_fact", "Propose one fact about the database for a person to "+
		"confirm (it binds nothing until then): who owns an object, test-fixture schemas, "+
		"a slot's consumer, an append-only table or a window. Cite the evidence ids "+
		"(e.g. finding:42) you read. At most once per question.",
		`"type":{"type":"string","enum":["owned_by_app_migrations","test_fixture",`+
			`"slot_consumer","append_only","table_window"]},"subject_kind":{"type":"string",`+
			`"enum":["index","table","schema","slot"]},"subject":{"type":"string",`+
			`"maxLength":300},"value":{"type":"object"},"rationale":{"type":"string",`+
			`"maxLength":500},"evidence_ids":{"type":"array","minItems":1,"maxItems":6,`+
			`"items":{"type":"string"}}`, []string{"type", "subject_kind", "subject",
			"evidence_ids"}, ss.proposeFact)
}

type proposeFactArgs struct {
	Type        string            `json:"type"`
	SubjectKind string            `json:"subject_kind"`
	Subject     string            `json:"subject"`
	Value       map[string]string `json:"value"`
	Rationale   string            `json:"rationale"`
	EvidenceIDs []string          `json:"evidence_ids"`
}

func (ss *session) proposeFact(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a proposeFactArgs
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	cites, err := ss.factCitations(a.EvidenceIDs)
	if err != nil {
		return agentloop.Output{}, err
	}
	if !ss.claimOnce(&ss.factDone) {
		return agentloop.Output{}, invalidArgs("one fact per question")
	}
	fact, created, err := facts.NewStore(ss.s.d.Pool).Propose(ctx, facts.Proposal{
		Type: facts.Type(a.Type), Kind: facts.Kind(a.SubjectKind), Subject: a.Subject,
		Value: a.Value, Source: facts.SourceModel, ProposedBy: "ask:" + ss.caller.Actor,
		Evidence: cites, Rationale: a.Rationale})
	if err != nil {
		return ss.factRefused(a.Subject, err)
	}
	if fact.Status != facts.StatusProposed {
		return ss.factRefused(a.Subject, fmt.Errorf("%w: fact %d is %s; Ask Sage does not "+
			"change a decided fact", ErrRefused, fact.ID, fact.Status))
	}
	act := ActionTaken{Kind: ActionFact, ID: fmt.Sprint(fact.ID), Status: ActionProposedFact,
		EvidenceID: fmt.Sprintf("fact:%d", fact.ID)}
	if !created {
		act.Status = ActionPending
	}
	ss.record(act)
	return ss.cite(act.EvidenceID, act.Status, fmt.Sprintf("fact %d %s: %s (%s). A person "+
		"confirms or rejects it on the Facts page; until then it binds nothing.", fact.ID,
		act.Status, fact.Describe(), fact.Provenance())), nil
}

// factCitations turns evidence ids this run read into fact citations;
// an id the run did not read is refused.
func (ss *session) factCitations(ids []string) ([]facts.Citation, error) {
	if len(ids) == 0 || len(ids) > maxFactEvidence {
		return nil, invalidArgs("cite 1-%d evidence ids read in this conversation turn",
			maxFactEvidence)
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]facts.Citation, 0, len(ids))
	for _, id := range ids {
		ev, ok := ss.evidence[id]
		if !ok {
			return nil, invalidArgs("evidence %q was not read in this turn", id)
		}
		kind, _, _ := strings.Cut(id, ":")
		out = append(out, facts.Citation{Kind: "ask_sage:" + kind, Ref: id,
			Detail: clip(ev.Label+" sha256:"+ev.Digest, 300)})
	}
	return out, nil
}

// factRefused reports a fact the store refused (invalid or protected
// subject) or one already decided; other failures are tool errors.
func (ss *session) factRefused(subject string, err error) (agentloop.Output, error) {
	for _, e := range []error{facts.ErrInvalidType, facts.ErrInvalidKind,
		facts.ErrInvalidSubject, facts.ErrProtectedSubject, facts.ErrInvalidValue,
		facts.ErrNoEvidence} {
		if errors.Is(err, e) {
			err = fmt.Errorf("%w: %v", ErrRefused, err)
			break
		}
	}
	return ss.refusedWrite(ActionFact, subject, err)
}
