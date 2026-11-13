// Package provideradvice reads the managed provider's own advisor
// recommendations (the Cloud SQL / AlloyDB index advisor, Azure index
// tuning, Supabase performance advisors) and reconciles them with
// pg_sage's open index findings (market-09).
//
// Provider advice is evidence only. A row says whether pg_sage agrees, and
// for provider-only advice whether pg_sage would act and why; it never
// raises a finding's confidence, risk tier or autonomy, and nothing here
// is read by the executor. Any action it leads to is an ordinary pg_sage
// candidate through the policy gate.
package provideradvice

import (
	"context"
	"errors"
	"time"
)

// Kind is what a recommendation asks for.
type Kind string

const (
	KindCreateIndex Kind = "create_index"
	KindDropIndex   Kind = "drop_index"
	// KindUnknown is advice pg_sage cannot read: an unrecognized view
	// shape or recommendation type. It is shown, never parsed.
	KindUnknown Kind = "unknown"
)

// Verdicts of a reconcile.
const (
	VerdictAgree        = "agree"
	VerdictPGSageOnly   = "pgsage_only"
	VerdictProviderOnly = "provider_only"
	VerdictConflict     = "conflict"
	VerdictUnparsed     = "unparsed"
	VerdictUnknown      = "unknown"
)

// Recommendation is one piece of provider advice as the provider gave it.
// Object is schema.table for an index to create and schema.index for one
// to drop; DDL is the provider's statement, unvalidated.
type Recommendation struct {
	Provider   string
	ProviderID string
	Kind       Kind
	Object     string
	DDL        string
	// Constraint is the foreign key the provider wants indexed when it
	// names no index definition (Supabase unindexed_foreign_keys).
	Constraint string
	// Indexes are a duplicate set the provider wants reduced to one.
	Indexes    []string
	Reason     string
	ObservedAt time.Time
}

// Source reads one provider's advice for one database.
type Source interface {
	Provider() string
	Fetch(ctx context.Context) ([]Recommendation, error)
}

// Source errors, wrapped with the provider's status and message.
var (
	// ErrAuth: the credential is missing, rejected or lacks the read role.
	ErrAuth = errors.New("provider advice: access denied")
	// ErrProvider: the provider API failed or could not be reached.
	ErrProvider = errors.New("provider advice: provider request failed")
	// ErrMalformed: the response could not be read.
	ErrMalformed = errors.New("provider advice: malformed response")
)
