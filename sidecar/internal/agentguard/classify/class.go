// Package classify keeps column classifications for agent governance
// (spec §6.7): secret, pii, untrusted_input and clean, stored as facts of
// type column_class keyed by (relid, attnum) so a rename keeps its class.
// Deterministic name and type rules and, with a model configured, the
// model propose classes (§6.16); an operator confirms them. Proposals may
// only narrow: a pending secret or pii proposal already counts, a clean
// one never does. Grant eligibility per environment lives here so the
// grant path enforces one rule: stage and prod grant only confirmed,
// classified columns; secret is never granted.
package classify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Class is a column classification.
type Class string

// Classes. Unclassified is the absence of a binding class.
const (
	ClassClean     Class = "clean"
	ClassUntrusted Class = "untrusted_input"
	ClassPII       Class = "pii"
	ClassSecret    Class = "secret"
	Unclassified   Class = ""
)

var narrowness = map[Class]int{ClassClean: 0, ClassUntrusted: 1, ClassPII: 2,
	ClassSecret: 3}

// ParseClass reads a class exactly as written.
func ParseClass(s string) (Class, error) {
	if _, ok := narrowness[Class(s)]; !ok {
		return "", fmt.Errorf("%w: got %q", ErrInvalidClass, s)
	}
	return Class(s), nil
}

// Narrowness orders classes by how much they restrict: clean 0 … secret 3;
// unclassified is -1.
func (c Class) Narrowness() int {
	if n, ok := narrowness[c]; ok {
		return n
	}
	return -1
}

// Status is where a classification stands (the facts statuses).
type Status string

// Statuses.
const (
	StatusProposed  Status = "proposed"
	StatusConfirmed Status = "confirmed"
	StatusRejected  Status = "rejected"
	StatusExpired   Status = "expired"
)

// Source is who proposed a classification.
type Source string

// Sources.
const (
	SourceOperator Source = "operator"
	SourceDetector Source = "detector"
	SourceModel    Source = "model"
)

// Proposer names.
const (
	RulesProposer = "classification rules"
	ModelProposer = "model"
)

// Errors. Each is distinguishable with errors.Is.
var (
	ErrInvalidClass = errors.New(
		"classify: class must be clean, pii, secret or untrusted_input")
	ErrInvalidStatus     = errors.New("classify: unknown status")
	ErrInvalidSource     = errors.New("classify: proposals come from rules or the model")
	ErrInvalidActor      = errors.New("classify: a decision names who made it")
	ErrNoEvidence        = errors.New("classify: a proposal must cite evidence")
	ErrColumnNotFound    = errors.New("classify: no such user column")
	ErrUnknownColumn     = errors.New("classify: the model named a column it was not shown")
	ErrNotFound          = errors.New("classify: classification not found")
	ErrInvalidTransition = errors.New("classify: only a proposed classification can be decided")
	ErrChanged           = errors.New("classify: the classification changed since it was shown")
	ErrModelOutput       = errors.New("classify: model output is not a classification list")
	ErrNoStore           = errors.New("classify: no database connection")
)

// Column is one user column (AttNum 0 stands for the whole table).
type Column struct {
	RelID   uint32 `json:"relid"`
	AttNum  int16  `json:"attnum"`
	Schema  string `json:"schema"`
	Table   string `json:"table"`
	Name    string `json:"column"`
	Type    string `json:"type"`
	Comment string `json:"comment,omitempty"`
}

// QualifiedName is schema.table.column (schema.table for a table).
func (c Column) QualifiedName() string {
	if c.AttNum == 0 {
		return c.Schema + "." + c.Table
	}
	return c.Schema + "." + c.Table + "." + c.Name
}

// Citation is one piece of evidence behind a proposal.
type Citation struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Detail string `json:"detail,omitempty"`
}

// Proposal is a class proposed for a column, before it is stored.
type Proposal struct {
	Column     Column
	Class      Class
	Source     Source
	ProposedBy string
	Evidence   []Citation
	Rationale  string
}

// Classification is one stored class.
type Classification struct {
	ID           int64      `json:"id"`
	Column       Column     `json:"column"`
	Class        Class      `json:"class"`
	Status       Status     `json:"status"`
	Source       Source     `json:"source"`
	ProposedBy   string     `json:"proposed_by"`
	Evidence     []Citation `json:"evidence"`
	Rationale    string     `json:"rationale"`
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
	Proposals    int        `json:"proposals"`
	Live         bool       `json:"live"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Effective is the class that binds now: a confirmed class, or a pending
// proposal that narrows (untrusted_input, pii, secret). A pending clean,
// a rejection or an expiry binds nothing.
func (c Classification) Effective() Class {
	switch c.Status {
	case StatusConfirmed:
		return c.Class
	case StatusProposed:
		if c.Class.Narrowness() > ClassClean.Narrowness() {
			return c.Class
		}
	}
	return Unclassified
}

// Hash binds a decision to what was shown: identity, class and status.
func (c Classification) Hash() string {
	b := fmt.Sprintf("%d\x1f%d\x1f%d\x1f%s\x1f%s", c.ID, c.Column.RelID, c.Column.AttNum,
		c.Class, c.Status)
	sum := sha256.Sum256([]byte(b))
	return hex.EncodeToString(sum[:16])
}

// Effective class of one column, with whether it rests on a confirmation.
type Effective struct {
	Class     Class `json:"class"`
	Confirmed bool  `json:"confirmed"`
	FactID    int64 `json:"fact_id,omitempty"`
}

// RelationClasses are the classifications of one relation.
type RelationClasses struct {
	RelID   uint32
	Table   *Classification
	Columns map[int16]Classification
}

// Of is the column's effective class: the narrower of its own class and
// the table's (a table-wide untrusted_input covers every column).
func (r RelationClasses) Of(attnum int16) Effective {
	var out Effective
	consider := func(c Classification) {
		eff := c.Effective()
		if eff == Unclassified || eff.Narrowness() < out.Class.Narrowness() {
			return
		}
		if eff == out.Class && out.Confirmed {
			return
		}
		out = Effective{Class: eff, Confirmed: c.Status == StatusConfirmed, FactID: c.ID}
	}
	if c, ok := r.Columns[attnum]; ok {
		consider(c)
	}
	if r.Table != nil {
		consider(*r.Table)
	}
	return out
}
