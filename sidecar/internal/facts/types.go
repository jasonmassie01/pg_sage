// Package facts keeps the typed facts about a monitored database that bind
// what pg_sage does (roadmap 2.3, "ownership and memory as binding facts").
//
// The model and deterministic detectors propose facts with cited evidence;
// an operator confirms or rejects each once (UI, API, MCP or a chat card).
// Only confirmed facts bind, and they can only narrow or redirect action,
// never widen autonomy: an object owned by the application's migrations is
// never changed by pg_sage (the change becomes a source-fix packet), test
// fixtures leave findings and budgets, a CDC slot is never dropped or
// advanced, an archive keeps its data and indexes, and a table's window
// holds work until it opens. The policy gate enforces this by typed
// matching (Binder); prompts carry confirmed facts as bounded context.
package facts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Type is what a fact says about its subject.
type Type string

// Fact types.
const (
	TypeAppMigrations Type = "owned_by_app_migrations"
	TypeTestFixture   Type = "test_fixture"
	TypeSlotConsumer  Type = "slot_consumer"
	TypeAppendOnly    Type = "append_only"
	TypeTableWindow   Type = "table_window"
)

// Kind is the catalog kind of a fact's subject.
type Kind string

// Subject kinds. KindRelation is only an object reference whose kind is
// unknown (a table or an index); no fact has it as its subject kind.
const (
	KindIndex    Kind = "index"
	KindTable    Kind = "table"
	KindSchema   Kind = "schema"
	KindSlot     Kind = "slot"
	KindRelation Kind = "relation"
)

// Status is where a fact stands. Only StatusConfirmed binds.
type Status string

// Fact statuses.
const (
	StatusProposed  Status = "proposed"
	StatusConfirmed Status = "confirmed"
	StatusRejected  Status = "rejected"
	StatusExpired   Status = "expired"
)

// Source is who proposed a fact.
type Source string

// Fact sources.
const (
	SourceModel    Source = "model"
	SourceOperator Source = "operator"
	SourceDetector Source = "detector"
)

// Errors. Each is distinguishable with errors.Is.
var (
	ErrInvalidType       = errors.New("facts: unknown fact type")
	ErrInvalidKind       = errors.New("facts: subject kind not allowed for this fact type")
	ErrInvalidSubject    = errors.New("facts: invalid subject")
	ErrProtectedSubject  = errors.New("facts: subject could match pg_sage's or a system schema")
	ErrInvalidValue      = errors.New("facts: invalid value")
	ErrInvalidSource     = errors.New("facts: unknown source")
	ErrNoEvidence        = errors.New("facts: a proposal must cite evidence")
	ErrNotFound          = errors.New("facts: fact not found")
	ErrInvalidTransition = errors.New("facts: decision not allowed in the fact's status")
	ErrChanged           = errors.New("facts: the fact changed since it was shown")
	ErrModelOutput       = errors.New("facts: model output is not a fact list")
)

// Citation is one piece of evidence behind a proposal.
type Citation struct {
	Kind       string    `json:"kind"`
	Ref        string    `json:"ref"`
	Detail     string    `json:"detail,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// Proposal is a fact as proposed, before the store keeps it.
type Proposal struct {
	Type       Type
	Kind       Kind
	Subject    string
	Value      map[string]string
	Source     Source
	ProposedBy string
	Evidence   []Citation
	Rationale  string
	ExpiresAt  *time.Time
}

// Fact is one stored fact.
type Fact struct {
	ID             int64             `json:"id"`
	Type           Type              `json:"type"`
	Kind           Kind              `json:"subject_kind"`
	Subject        string            `json:"subject"`
	Value          map[string]string `json:"value"`
	Source         Source            `json:"source"`
	ProposedBy     string            `json:"proposed_by"`
	Evidence       []Citation        `json:"evidence"`
	Rationale      string            `json:"rationale"`
	Status         Status            `json:"status"`
	DecidedBy      string            `json:"decided_by,omitempty"`
	DecidedAt      *time.Time        `json:"decided_at,omitempty"`
	DecisionNote   string            `json:"decision_note,omitempty"`
	ExpiresAt      *time.Time        `json:"expires_at,omitempty"`
	ExpiredReason  string            `json:"expired_reason,omitempty"`
	Proposals      int               `json:"proposals"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	LastVerifiedAt *time.Time        `json:"last_verified_at,omitempty"`
}

// Decision is an operator's confirm or reject of a fact. ExpectHash, when
// set, binds the decision to the fact as it was shown (a chat card).
type Decision struct {
	Confirm    bool
	Actor      string
	Note       string
	ExpectHash string
}

// Filter selects facts to list; zero values select everything.
type Filter struct {
	Status []Status
	Type   Type
	Limit  int
}

// kindNames are a subject kind in words.
var kindNames = map[Kind]string{KindIndex: "index", KindTable: "table",
	KindSchema: "schemas", KindSlot: "replication slot"}

// Describe is the fact in words, e.g. "index public.idx_a is owned by the
// application's migrations".
func (f Fact) Describe() string {
	subject := kindNames[f.Kind] + " " + f.Subject
	switch f.Type {
	case TypeAppMigrations:
		if f.Kind == KindSchema {
			subject = "schema " + f.Subject
		}
		return subject + " is owned by the application's migrations"
	case TypeTestFixture:
		return subject + " are test fixtures"
	case TypeSlotConsumer:
		return subject + " belongs to " + f.Value["consumer"]
	case TypeAppendOnly:
		return subject + " is append-only (archive)"
	case TypeTableWindow:
		return fmt.Sprintf("%s has a %s window: %s", subject, f.Value["kind"],
			f.Value["window"])
	}
	return subject + " " + string(f.Type)
}

// Provenance names the fact and its decision: "fact #12, confirmed by
// alice@example.com on 2026-10-04".
func (f Fact) Provenance() string {
	out := fmt.Sprintf("fact #%d", f.ID)
	if f.DecidedBy == "" || f.DecidedAt == nil {
		return out + ", " + string(f.Status)
	}
	return fmt.Sprintf("%s, %s by %s on %s", out, f.Status, f.DecidedBy,
		f.DecidedAt.UTC().Format("2006-01-02"))
}

// Hash binds a decision to what was shown: the fact's identity, value and
// status (not its proposal count or evidence, which grow as it is
// re-proposed).
func (f Fact) Hash() string {
	keys := make([]string, 0, len(f.Value))
	for k := range f.Value {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%d\x1f%s\x1f%s\x1f%s\x1f%s", f.ID, f.Type, f.Kind, f.Subject, f.Status)
	for _, k := range keys {
		fmt.Fprintf(&b, "\x1f%s=%s", k, f.Value[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16])
}

// binds reports whether the fact is confirmed and unexpired at now.
func (f Fact) binds(now time.Time) bool {
	return f.Status == StatusConfirmed && (f.ExpiresAt == nil || f.ExpiresAt.After(now))
}
