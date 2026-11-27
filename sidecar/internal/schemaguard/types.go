package schemaguard

import "time"

type InvariantKind string

// Every kind has a detector (a non-test producer); a kind without one is
// not declared (see producer_guard_test.go).
const (
	InvariantMissingFKIndex  InvariantKind = "missing_fk_index"
	InvariantUnboundedAppend InvariantKind = "unbounded_append"
	InvariantEverythingText  InvariantKind = "everything_text"
	InvariantTypeTightening  InvariantKind = "type_tightening"
)

type RemediationClass string

const (
	RemediationVerifiedIndex RemediationClass = "verified_index"
	RemediationRetention     RemediationClass = "retention"
	RemediationStructural    RemediationClass = "structural"
)

type Classification struct {
	Class                 RemediationClass
	AutoRemediable        bool
	RequiresVerification  bool
	RequiresRehearsal     bool
	RequiresPolicyConsent bool
}
type Disposition string

const (
	DispositionApply     Disposition = "apply"
	DispositionRecommend Disposition = "recommend"
	DispositionDryRun    Disposition = "dry_run"
	DispositionPark      Disposition = "park"
)

type Route string

// RouteRecommendation is advisory only: the decision is recorded for the
// operator and never sent to a router.
const (
	RouteVerifyIndex    Route = "verify_index"
	RouteRetention      Route = "retention"
	RouteRecommendation Route = "recommendation"
)

// StructuralReason is the recorded reason of every structural invariant:
// fixing one needs an online column migration, which pg_sage cannot run.
const StructuralReason = "requires an online column migration; " +
	"pg_sage has no automated path"

// Invariant is one detected schema condition. For unbounded-append tables,
// RetentionColumn is the owner-declared column, set only when it exists, is
// not dropped and is timestamptz, timestamp or date; RetentionSuggestion is
// pg_sage's advisory guess, shown to the owner and never acted on. Subject
// names what on the table the invariant is about (a constraint or a
// column); empty when the table itself is the subject. Family is the
// clone-schema family the table's schema belongs to, or nil.
type Invariant struct {
	Kind                         InvariantKind
	Schema, Table, ProposedSQL   string
	RollbackSQL, RetentionColumn string
	RetentionSuggestion          string
	Subject                      string
	QueryIDs                     []int64
	Family                       *Family
}

// Target is the schema-qualified table, "schema.table".
func (i Invariant) Target() string { return i.Schema + "." + i.Table }

// Family is a set of schemas that are copies of one shape (same tables,
// generated name suffix, at least FamilyMinMembers copies). Idle families
// are leftovers (no scans, writes, statements or sessions); live ones are
// e.g. schema-per-tenant. Reason is the evidence for the classification.
type Family struct {
	Key     string
	Members []string
	Idle    bool
	Reason  string
}

// TableContract is the owner's declaration. RetentionColumn is the declared
// retention clock; empty means undeclared (a pre-D5 contract).
type TableContract struct {
	AppendOnly         bool
	RetentionWindow    time.Duration
	RetentionColumn    string
	ExpectedPrimaryKey string
	Exemptions         []InvariantKind
}
type Policy struct {
	AllowFKIndexApply   bool
	AllowRetentionApply bool
	OscillationLimit    int
}
type History struct{ SuccessfulRetentionDryRuns, ExternalReversions int }

// HistoryKey is one invariant kind on one schema-qualified table.
type HistoryKey struct {
	Kind   InvariantKind
	Target string
}

// HistoryIndex is the recorded history of one scan's invariants, read in
// one lookup: remediation counts per kind and table, and the decision hash
// last recorded for each invariant identity.
type HistoryIndex struct {
	ByTarget map[HistoryKey]History
	LastHash map[string]string
}

// For returns the remediation history of one invariant.
func (h HistoryIndex) For(invariant Invariant) History {
	return h.ByTarget[HistoryKey{Kind: invariant.Kind, Target: invariant.Target()}]
}

type Request struct {
	Invariant Invariant
	Contract  TableContract
	Policy    Policy
	History   History
}
type Decision struct {
	Class                RemediationClass
	Disposition          Disposition
	Route                Route
	Reason               string
	RequiresVerification bool
	RequiresRehearsal    bool
	AutoApply            bool
	MayDeleteData        bool
}

func DefaultPolicy() Policy { return Policy{OscillationLimit: 3} }
