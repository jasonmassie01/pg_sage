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
