package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// The only two writes Ask Sage can make, offered only to callers who may
// propose: open an investigation (read-only probes; it changes nothing)
// and queue one of pg_sage's own findings for a person's approval. Each
// runs at most once per question. Neither can execute or approve: the
// policy gate decides the proposal's verdict and a person approves it on
// the Actions page. Arguments are typed ids and a subject, never SQL.

const maxSubjectRunes = 200

func (ss *session) proposeTool() agentloop.Tool {
	return tool("propose_action", "Queue one of pg_sage's open findings for a person's "+
		"approval: its own SQL, rollback and predicted effect, with the policy gate's "+
		"verdict. Never executes; a person approves on the Actions page. At most once "+
		"per question.", `"finding_id":{"type":"integer","minimum":1}`,
		[]string{"finding_id"}, ss.propose)
}

func (ss *session) propose(ctx context.Context, raw json.RawMessage) (agentloop.Output,
	error) {
	var a struct {
		FindingID int64 `json:"finding_id"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	if a.FindingID < 1 {
		return agentloop.Output{}, invalidArgs("finding_id must be a positive integer")
	}
	if !ss.claimOnce(&ss.proposed) {
		return agentloop.Output{}, invalidArgs("one proposal per question")
	}
	p, err := ss.s.d.Proposer.ProposeFinding(ctx, a.FindingID, "ask:"+ss.caller.Actor)
	if err != nil {
		return ss.refusedWrite(ActionProposal, fmt.Sprintf("finding %d", a.FindingID), err)
	}
	act := ActionTaken{Kind: ActionProposal, ID: fmt.Sprint(p.QueueID),
		Status: ActionQueued, Verdict: p.Verdict, Reason: p.Reason, SQL: p.SQL,
		RollbackSQL: p.RollbackSQL, RollbackClass: p.RollbackClass,
		Prediction: p.Prediction, EvidenceID: fmt.Sprintf("proposal:%d", p.QueueID)}
	if !p.Created {
		act.Status = ActionPending
	}
	ss.record(act)
	return ss.cite(act.EvidenceID, act.Status, proposalText(p, act.Status)), nil
}

func proposalText(p Proposal, status string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "proposal %d for finding %d: %s; policy verdict %s (%s), risk %s, "+
		"action %s\nSQL: %s\n", p.QueueID, p.FindingID, status, p.Verdict, p.Reason,
		p.RiskTier, p.ActionType, p.SQL)
	fmt.Fprintf(&b, "rollback (%s): %s\n", p.RollbackClass, p.RollbackSQL)
	if len(p.Prediction) > 0 {
		fmt.Fprintf(&b, "predicted effect: %s\n", p.Prediction)
	}
	b.WriteString("A person approves or rejects it on the Actions page; Ask Sage cannot.")
	return b.String()
}

func (ss *session) openInvestigationTool() agentloop.Tool {
	return tool("open_investigation", "Open an investigation of a symptom (read-only "+
		"probes; it changes nothing). At most once per question.", `"subject":{"type":`+
		`"string","minLength":1,"maxLength":200,"description":"the symptom, e.g. `+
		`checkout latency since 14:00"}`, []string{"subject"}, ss.openInvestigation)
}

func (ss *session) openInvestigation(ctx context.Context, raw json.RawMessage) (
	agentloop.Output, error) {
	var a struct {
		Subject string `json:"subject"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return agentloop.Output{}, err
	}
	subject := oneLine(a.Subject)
	if subject == "" || utf8.RuneCountInString(subject) > maxSubjectRunes {
		return agentloop.Output{}, invalidArgs("subject must be 1-%d characters",
			maxSubjectRunes)
	}
	if !ss.claimOnce(&ss.opened) {
		return agentloop.Output{}, invalidArgs("one investigation per question")
	}
	actor := "ask:" + ss.caller.Actor
	started, err := ss.s.d.Starter.StartInvestigation(ctx, StartRequest{Subject: subject,
		CaseID: "ask:" + digestOf(actor + "\n" + subject)[:16], Actor: actor})
	if err != nil {
		return ss.refusedWrite(ActionInvestigation, subject, err)
	}
	act := ActionTaken{Kind: ActionInvestigation, ID: started.ID, Status: ActionOpened,
		EvidenceID: "investigation:" + started.ID}
	if !started.Created {
		act.Status = ActionJoined
	}
	ss.record(act)
	return ss.cite(act.EvidenceID, act.Status, fmt.Sprintf("investigation %s %s for %q; "+
		"it runs in the background with read-only probes and changes nothing",
		started.ID, act.Status, subject)), nil
}

// claimOnce marks a write as used for this question; false if it was.
func (ss *session) claimOnce(flag *bool) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if *flag {
		return false
	}
	*flag = true
	return true
}

// refusedWrite records a write that did not happen: a refusal or a block
// is told to the model as a result; any other failure is a tool error.
func (ss *session) refusedWrite(kind, what string, err error) (agentloop.Output, error) {
	act := ActionTaken{Kind: kind, Status: ActionFailed, Reason: clip(err.Error(), 300)}
	switch {
	case errors.Is(err, ErrBlocked):
		act.Status = ActionBlocked
	case errors.Is(err, ErrRefused):
		act.Status = ActionRefused
	}
	ss.record(act)
	if act.Status == ActionFailed {
		return agentloop.Output{}, fmt.Errorf("the %s for %s failed: %w", kind, what, err)
	}
	return agentloop.Output{Status: act.Status, Text: fmt.Sprintf("the %s for %s was "+
		"%s: %s", kind, what, act.Status, act.Reason)}, nil
}
