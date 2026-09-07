package schemaguard

import "time"

type InvariantKind string

const (
	InvariantMissingFKIndex    InvariantKind = "missing_fk_index"
	InvariantUnboundedAppend   InvariantKind = "unbounded_append"
	InvariantMissingConstraint InvariantKind = "missing_constraint"
	InvariantEverythingText    InvariantKind = "everything_text"
	InvariantTypeTightening    InvariantKind = "type_tightening"
	InvariantRandomUUIDPK      InvariantKind = "random_uuid_pk"
	InvariantNoPrimaryKey      InvariantKind = "no_primary_key"
	InvariantRedundantIndex    InvariantKind = "redundant_index"
)

type RemediationClass string

const (
	RemediationVerifiedIndex  RemediationClass = "verified_index"
	RemediationRetention      RemediationClass = "retention"
	RemediationRecommendation RemediationClass = "recommendation"
	RemediationStructural     RemediationClass = "structural"
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

const (
	RouteVerifyIndex    Route = "verify_index"
	RouteRetention      Route = "retention"
	RouteCloneRehearsal Route = "clone_rehearsal"
	RouteRecommendation Route = "recommendation"
)

type Invariant struct {
	Kind                         InvariantKind
	Schema, Table, ProposedSQL   string
	RollbackSQL, RetentionColumn string
	QueryIDs                     []int64
}
type TableContract struct {
	AppendOnly         bool
	RetentionWindow    time.Duration
	ExpectedPrimaryKey string
	Exemptions         []InvariantKind
}
type Policy struct {
	AllowFKIndexApply          bool
	AllowRetentionApply        bool
	AllowRedundantIndexCleanup bool
	OscillationLimit           int
}
type History struct{ SuccessfulRetentionDryRuns, ExternalReversions int }
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
