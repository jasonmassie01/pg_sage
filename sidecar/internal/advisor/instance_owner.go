package advisor

import (
	"context"
	"maps"
	"regexp"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// In fleet mode several databases can sit on one PostgreSQL instance
// (advisors-08). The instance-level questions (WAL, connections) and the
// changes that apply to the whole instance (ALTER SYSTEM, a managed
// parameter group or database flag) belong to one owner database, so N
// databases pay one model call and never race N different values.

// DetailInstanceOwner names, on an instance-level finding of a database
// that does not own its instance, the database whose advisor manages the
// instance's settings.
const DetailInstanceOwner = "instance_owner"

// InstanceOwner reports the database that owns this database's instance
// settings and whether this database is it. An empty owner means the
// instance identity is unknown: the database is advised on its own.
type InstanceOwner func() (owner string, isOwner bool)

// instanceCategories are the advisor categories about the whole instance.
var instanceCategories = map[string]bool{"wal_tuning": true, "connection_tuning": true}

// alterSystemPattern finds an ALTER SYSTEM statement anywhere in a
// finding's SQL, however it is spaced or cased, so a multi-statement
// proposal cannot slip past the instance gate.
var alterSystemPattern = regexp.MustCompile(`(?i)\balter\s+system\b`)

// WithInstanceOwner sets how this advisor learns whether its database
// owns the instance settings. Nil (the default) advises the database on
// its own, as in single-database mode.
func (a *Advisor) WithInstanceOwner(fn InstanceOwner) {
	a.instanceOwner = fn
}

// ownership is one cycle's view of the instance owner.
type ownership struct {
	owner   string
	isOwner bool
}

// defers reports whether this database leaves instance settings to
// another database.
func (o ownership) defers() bool { return !o.isOwner && o.owner != "" }

func (a *Advisor) ownership() ownership {
	if a.instanceOwner == nil {
		return ownership{isOwner: true}
	}
	owner, isOwner := a.instanceOwner()
	return ownership{owner: owner, isOwner: isOwner}
}

type ownershipKey struct{}

func withOwnership(ctx context.Context, o ownership) context.Context {
	return context.WithValue(ctx, ownershipKey{}, o)
}

// ownershipFrom is the cycle's ownership; outside a cycle the database
// owns its instance (single-database behaviour).
func ownershipFrom(ctx context.Context) ownership {
	if o, ok := ctx.Value(ownershipKey{}).(ownership); ok {
		return o
	}
	return ownership{isOwner: true}
}

// GateInstanceSettings makes every instance-wide change advisory on a
// database that does not own its instance: an ALTER SYSTEM statement or
// a managed parameter change loses its SQL and intent and names the
// owner. Database-scope advice (ALTER DATABASE, maintenance, rewrites) is
// unchanged, as is everything on the owner or when the owner is unknown.
func GateInstanceSettings(findings []analyzer.Finding, owner string,
	isOwner bool) []analyzer.Finding {
	if !(ownership{owner: owner, isOwner: isOwner}).defers() {
		return findings
	}
	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		if instanceScoped(f) {
			f = deferToOwner(f, owner)
		}
		out = append(out, f)
	}
	return out
}

// instanceScoped reports a finding whose change applies to the instance.
func instanceScoped(f analyzer.Finding) bool {
	if alterSystemPattern.MatchString(f.RecommendedSQL) {
		return true
	}
	_, managed := managedparam.IntentFromDetail(f.Detail)
	return managed
}

// deferToOwner turns an instance-wide change into guidance naming owner.
func deferToOwner(f analyzer.Finding, owner string) analyzer.Finding {
	f.Detail = maps.Clone(f.Detail)
	if f.Detail == nil {
		f.Detail = map[string]any{}
	}
	delete(f.Detail, managedparam.DetailKey)
	delete(f.Detail, analyzer.DetailApprovalRequired)
	f.Detail[DetailInstanceOwner] = owner
	return notExecutable(f, "instance settings are managed from database "+owner)
}

// splitInstanceOpen sets aside the open instance-level findings this
// cycle must not re-check. A non-owner sets aside all of them: they stay
// open, advisory and naming the owner, and only the owner resolves them.
// The owner sets aside the rows it deferred while it was not the owner,
// so they leave the open gate and its own review can replace them.
func splitInstanceOpen(open []analyzer.Finding, o ownership) (checked,
	aside []analyzer.Finding) {
	for _, f := range open {
		_, deferred := f.Detail[DetailInstanceOwner]
		if instanceCategories[f.Category] && (o.defers() || deferred) {
			aside = append(aside, f)
			continue
		}
		checked = append(checked, f)
	}
	return checked, aside
}

// keepAside re-emits the set-aside findings: deferred to the owner on a
// non-owner. On the owner a deferred row stays open until the owner's own
// review of its category has run (its setting was in that prompt, since
// the row left the open gate): the review's answer then replaces the row,
// or resolves it when the model advised nothing for it.
func keepAside(aside []analyzer.Finding, r subRun, o ownership) []analyzer.Finding {
	if o.defers() {
		return deferAside(aside, o.owner)
	}
	var out []analyzer.Finding
	for _, f := range aside {
		if !r.ran[f.Category] {
			out = append(out, f)
		}
	}
	return out
}

// deferAside makes a non-owner's open instance rows advisory, each naming
// the owner.
func deferAside(aside []analyzer.Finding, owner string) []analyzer.Finding {
	out := GateInstanceSettings(aside, owner, false)
	for i, f := range out {
		if _, named := f.Detail[DetailInstanceOwner]; !named {
			f.Detail = maps.Clone(f.Detail)
			if f.Detail == nil {
				f.Detail = map[string]any{}
			}
			f.Detail[DetailInstanceOwner] = owner
			out[i] = f
		}
	}
	return out
}
