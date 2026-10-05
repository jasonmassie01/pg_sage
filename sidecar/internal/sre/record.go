package sre

import (
	"encoding/json"
	"errors"
	"time"
)

// HypothesisStatus is a hypothesis' place in a persisted diagnosis
// (root cause, contributing factor, unproven alternative, ruled out).
type HypothesisStatus string

// Hypothesis statuses, matching causal.Status.
const (
	HypothesisRoot         HypothesisStatus = "root_cause"
	HypothesisContributing HypothesisStatus = "contributing"
	HypothesisUnproven     HypothesisStatus = "unproven"
	HypothesisRuledOut     HypothesisStatus = "ruled_out"
)

func (s HypothesisStatus) valid() bool {
	switch s {
	case HypothesisRoot, HypothesisContributing, HypothesisUnproven, HypothesisRuledOut:
		return true
	}
	return false
}

// Fact is one observation bound to the stored evidence it came from.
type Fact struct {
	EvidenceID UUID   `json:"evidence_id"`
	Text       string `json:"text"`
}

// HypothesisRecord is one persisted hypothesis of one diagnosis revision.
type HypothesisRecord struct {
	ID              UUID             `json:"id"`
	Revision        int              `json:"revision"`
	Ordinal         int              `json:"ordinal"`
	GraphVersion    string           `json:"graph_version"`
	Family          string           `json:"family"`
	Node            string           `json:"node"`
	Label           string           `json:"label"`
	Mechanism       string           `json:"mechanism"`
	Subject         string           `json:"subject"`
	Status          HypothesisStatus `json:"status"`
	Confidence      float64          `json:"confidence"`
	Support         []Fact           `json:"support"`
	Contradict      []Fact           `json:"contradict"`
	RefutationProbe string           `json:"refutation_probe"`
	OperatorStep    string           `json:"operator_step,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
}

// MissingEvidence names evidence a diagnosis could not use; a failed or
// unavailable probe is reported here, never read as a healthy zero.
type MissingEvidence struct {
	ProbeID string `json:"probe_id"`
	Status  string `json:"status,omitempty"`
	Reason  string `json:"reason"`
}

// Summary is the persisted outcome of a diagnosis.
type Summary struct {
	Family       string            `json:"family,omitempty"`
	GraphVersion string            `json:"graph_version,omitempty"`
	Subject      string            `json:"subject,omitempty"`
	Conclusive   bool              `json:"conclusive"`
	Reason       string            `json:"reason,omitempty"`
	Root         string            `json:"root,omitempty"`
	Observed     []Fact            `json:"observed,omitempty"`
	Missing      []MissingEvidence `json:"missing,omitempty"`
	// Model output (M3), stored beside the deterministic diagnosis and
	// never merged into it: the model's ranking of the graph's open
	// hypotheses, its cited narrative and the one probe it asked for.
	ModelRanking *ModelRanking `json:"model_ranking,omitempty"`
	Narrative    *Narrative    `json:"narrative,omitempty"`
	ModelProbe   *ModelProbe   `json:"model_probe,omitempty"`
	// ModelContest (roadmap 2.4) is a model ranking that contested the
	// graph's conclusive root: advisory (the graph's root stands) unless
	// the family's root authority was earned on the held-out bench.
	ModelContest *ModelContest `json:"model_contest,omitempty"`
	// ModelConclusion and Investigator (roadmap 2.1) are the tool-calling
	// investigator's conclusion, with the authority it got, and its
	// transcript (plan, tool calls with evidence and digests, refusals).
	ModelConclusion *ModelConclusion `json:"model_conclusion,omitempty"`
	Investigator    *InvestigatorRun `json:"investigator,omitempty"`
	// CustomerImpact (M5) is the customer-impact claim bound to the SLO
	// status evidence; only a registered app SLI can claim it.
	CustomerImpact *CustomerImpact `json:"customer_impact,omitempty"`
	// Proposals are the custodian actions that address a conclusive runway
	// diagnosis (M6), with the gate's explained verdict; never executed here.
	Proposals []ActionProposal `json:"proposals,omitempty"`
	// M6: the signed runbook that ran (version, path, proposal) and the
	// similar past incidents the model turn was offered as context.
	Runbook *RunbookRun `json:"runbook,omitempty"`
	Memory  *MemoryRef  `json:"memory,omitempty"`
}

// Conclusion ends an investigation run: concluded (a supported root
// cause), inconclusive (a first-class outcome with a reason) or failed.
type Conclusion struct {
	State       State
	FailureCode string
	Summary     Summary
	Hypotheses  []HypothesisRecord
}

// Event types of the append-only hash chain.
const (
	EventCreated        = "created"
	EventClaimed        = "claimed"
	EventStep           = "step"
	EventTransition     = "transition"
	EventConcluded      = "concluded"
	EventPinned         = "pinned"
	EventUnpinned       = "unpinned"
	EventEvidencePurged = "evidence_purged"
	// M3 model turn: an accepted review, a fallback to the deterministic
	// result (with its reason) and a disagreement with a conclusive graph.
	EventModelReviewed  = "model_reviewed"
	EventModelRejected  = "model_rejected"
	EventModelDisagreed = "model_disagreed"
)

// Event is one link of an investigation's hash chain.
type Event struct {
	Sequence     int             `json:"sequence"`
	Type         string          `json:"type"`
	Actor        string          `json:"actor"`
	ObservedAt   time.Time       `json:"observed_at"`
	Payload      json.RawMessage `json:"payload"`
	PreviousHash []byte          `json:"previous_hash,omitempty"`
	Hash         []byte          `json:"hash"`
}

// ErrChainBroken reports an event chain whose hashes do not verify.
var ErrChainBroken = errors.New("event hash chain broken")

// ListFilter pages a scoped investigation list, newest first. Cursor is
// the opaque NextCursor of the previous page.
type ListFilter struct {
	Limit  int
	Cursor string
	// CaseID, when set, lists only the investigations of that case.
	CaseID string
}

// Page is one page of investigations.
type Page struct {
	Items      []Investigation
	NextCursor string
}

// Tombstone records what retention deleted.
type Tombstone struct {
	InvestigationID UUID            `json:"investigation_id"`
	Kind            string          `json:"kind"`
	DeletedAt       time.Time       `json:"deleted_at"`
	Reason          string          `json:"reason"`
	RowCount        int             `json:"row_count"`
	Detail          json.RawMessage `json:"detail"`
}

// RetentionPolicy ages out terminal, unpinned investigations: their
// evidence after EvidenceAge, the whole timeline after TimelineAge.
type RetentionPolicy struct {
	EvidenceAge time.Duration
	TimelineAge time.Duration
	BatchSize   int
}

// PurgeResult counts what one retention pass removed.
type PurgeResult struct {
	EvidencePurged       int
	InvestigationsPurged int
}
