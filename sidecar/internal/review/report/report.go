// Package report is Sage Review's report (review-03a): the ranked
// candidates, their links and hypotheses, and one text per dimension. The
// model pass fills it when a model is usable; without one it is built
// here from generator output alone, ranked by a deterministic score, with
// each dimension's text from a text/template. Either way it is a report
// for a person: nothing in it runs.
package report

import (
	"math"
	"sort"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// PassStatus is the model pass's state: pending, then running, then done,
// failed or skipped (a pass that never starts goes from pending to
// skipped or failed).
type PassStatus string

// Model pass states.
const (
	PassPending PassStatus = "pending"
	PassRunning PassStatus = "running"
	PassDone    PassStatus = "done"
	PassFailed  PassStatus = "failed"
	PassSkipped PassStatus = "skipped"
)

// Where an entry of the report came from.
const (
	OriginModel         = "model"
	OriginDeterministic = "deterministic"
)

// Consensus labels.
const (
	ConsensusMajority      = "majority"
	ConsensusRefute        = "refute"
	ConsensusSingle        = "single run"
	ConsensusDeterministic = "deterministic"
)

// Decisions a ranking may hold.
const (
	DecisionKeep          = "keep"
	DecisionDrop          = "drop"
	DecisionNeedsEvidence = "needs_evidence"
)

// Ranked is one kept candidate in rank order (1 is first). Votes is how
// many model runs kept it (0 for the deterministic ranking); Claims cite
// durable evidence ids.
type Ranked struct {
	CandidateID string            `json:"candidate_id"`
	Dimension   string            `json:"dimension"`
	Kind        string            `json:"kind"`
	Target      string            `json:"target"`
	ActionType  string            `json:"action_type"`
	Rank        int               `json:"rank"`
	Decision    string            `json:"decision"`
	Attack      string            `json:"attack,omitempty"`
	Score       float64           `json:"score"`
	Votes       int               `json:"votes,omitempty"`
	EvidenceIDs []string          `json:"evidence_ids"`
	Claims      []agentloop.Claim `json:"claims,omitempty"`
	Origin      string            `json:"origin"`
}

// Dropped is a candidate the model pass did not keep, and why.
type Dropped struct {
	CandidateID string `json:"candidate_id"`
	Reason      string `json:"reason"`
	Votes       int    `json:"votes"`
}

// Rejected is one entry of a model run's answer that was refused before
// consensus (an unknown candidate, an uncited link, ...).
type Rejected struct {
	Attempt int    `json:"attempt"`
	Item    string `json:"item"`
	Reason  string `json:"reason"`
}

// Link relates two candidates (conflicts, depends_on, supersedes,
// related).
type Link struct {
	A        string            `json:"a"`
	B        string            `json:"b"`
	Relation string            `json:"relation"`
	Claims   []agentloop.Claim `json:"claims"`
	Votes    int               `json:"votes"`
}

// Hypothesis is a change no generator proposed that the model suggests
// investigating; review-03b turns it into a typed step or nothing.
type Hypothesis struct {
	Title     string            `json:"title"`
	Kind      string            `json:"kind"`
	Target    string            `json:"target"`
	Rationale string            `json:"rationale,omitempty"`
	Claims    []agentloop.Claim `json:"claims"`
	Votes     int               `json:"votes"`
}

// Section is one dimension's text.
type Section struct {
	Dimension string            `json:"dimension"`
	Title     string            `json:"title"`
	Letter    string            `json:"letter,omitempty"`
	Text      string            `json:"text"`
	Claims    []agentloop.Claim `json:"claims,omitempty"`
	Origin    string            `json:"origin"`
}

// Report is one review's report.
type Report struct {
	Database        string                   `json:"database"`
	RubricVersion   string                   `json:"rubric_version,omitempty"`
	Overall         string                   `json:"overall,omitempty"`
	ModelPass       PassStatus               `json:"model_pass"`
	ModelPassReason string                   `json:"model_pass_reason,omitempty"`
	Consensus       string                   `json:"consensus"`
	Runs            int                      `json:"runs"`
	RunsDone        int                      `json:"runs_done"`
	Ranked          []Ranked                 `json:"ranked"`
	Dropped         []Dropped                `json:"dropped,omitempty"`
	Rejected        []Rejected               `json:"rejected,omitempty"`
	Links           []Link                   `json:"links,omitempty"`
	Hypotheses      []Hypothesis             `json:"hypotheses,omitempty"`
	Sections        []Section                `json:"sections"`
	DroppedClaims   []agentloop.DroppedClaim `json:"dropped_claims,omitempty"`
}

// tierConfidence is how far a prediction's lab-03 ladder tier backs it.
var tierConfidence = map[string]float64{"fleet": 1, "canary": 0.9, "clone": 0.75,
	"plan_only": 0.5, candidate.TierNone: 0.25}

// Score is the deterministic rank key: the predicted effect (the size in
// percent, 1 when unknown) times the ladder tier's confidence (an unknown
// tier counts as none), divided by the risk weight (1, plus 1 when the
// step needs approval, plus 1 when an executed step cannot be rolled
// back). It is rounded to 4 decimals so equal inputs tie exactly.
func Score(c candidate.Candidate) float64 {
	effect := 1.0
	if p := c.Prediction.SizePct; p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) &&
		*p != 0 {
		effect = math.Abs(*p)
	}
	conf, ok := tierConfidence[c.Prediction.Tier]
	if !ok {
		conf = tierConfidence[candidate.TierNone]
	}
	return math.Round(effect*conf/riskWeight(c)*1e4) / 1e4
}

func riskWeight(c candidate.Candidate) float64 {
	w := 1.0
	if c.Approval != candidate.ApprovalAutomatic {
		w++
	}
	executed := c.ActionType != candidate.ActionAdvice &&
		c.ActionType != candidate.ActionOperator
	if executed && !c.Reversible {
		w++
	}
	return w
}

// Deterministic ranks every candidate by Score, highest first, ties by
// id, and keeps them all.
func Deterministic(cands []candidate.Candidate) []Ranked {
	out := make([]Ranked, 0, len(cands))
	for _, c := range cands {
		r := FromCandidate(c)
		r.Decision, r.Origin = DecisionKeep, OriginDeterministic
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].CandidateID < out[j].CandidateID
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// FromCandidate is c's report entry before ranking.
func FromCandidate(c candidate.Candidate) Ranked {
	return Ranked{CandidateID: c.ID, Dimension: c.Dimension, Kind: c.Kind, Target: c.Target,
		ActionType: c.ActionType, Score: Score(c),
		EvidenceIDs: append([]string(nil), c.EvidenceIDs...)}
}
