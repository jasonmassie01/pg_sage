package facts

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// ConfirmedSource returns the confirmed, unexpired facts of a database.
type ConfirmedSource interface {
	Confirmed(context.Context) ([]Fact, error)
}

// RefResolver completes references from the catalog: a relation of
// unknown kind gets its kind, an index its table, and a partition (or a
// partition's index) the root of its partition tree (facts-10). It
// returns one ref per ref, in order.
type RefResolver interface {
	ResolveCatalog(context.Context, []ObjectRef) ([]ObjectRef, error)
}

// SiblingDatabase is another database of the same cluster and its
// confirmed facts.
type SiblingDatabase struct {
	Database string
	Facts    ConfirmedSource
}

// SiblingSource lists the sibling databases at the time of a request.
type SiblingSource func() []SiblingDatabase

// Binder is the policy gate's view of the confirmed facts
// (policy.FactBinder): typed matching of a request's objects against the
// confirmed fact subjects, never model text.
type Binder struct {
	source   ConfirmedSource
	resolver RefResolver
	siblings SiblingSource
	now      func() time.Time
}

// NewBinder binds requests with the facts from source; resolver may be nil
// (references are then matched as written); a resolver that is also a
// TargetResolver qualifies unqualified targets (binder_targets.go); now
// defaults to time.Now.
func NewBinder(source ConfirmedSource, resolver RefResolver, now func() time.Time) *Binder {
	if now == nil {
		now = time.Now
	}
	return &Binder{source: source, resolver: resolver, now: now}
}

// WithSiblings makes the confirmed slot_consumer facts of the sibling
// databases on the same cluster bind every request that touches a slot:
// slots and max_slot_wal_keep_size are cluster objects. Facts of other
// types never cross databases. A nil source reads no siblings.
func (b *Binder) WithSiblings(siblings SiblingSource) *Binder {
	b.siblings = siblings
	return b
}

// Bind implements policy.FactBinder.
func (b *Binder) Bind(ctx context.Context, req policy.ActionRequest) (
	[]policy.FactBinding, error) {
	facts, err := b.source.Confirmed(ctx)
	if err != nil {
		return nil, fmt.Errorf("read confirmed facts: %w", err)
	}
	r := Request{SQL: req.SQL, Targets: req.TargetObjs, Now: b.now(),
		OperatorApproved: req.OperatorApproved}
	if req.Contract != nil {
		r.ActionType = req.Contract.ActionType
	}
	sqlOnly, targets := sqlRefs(scanSQL(r.SQL)), targetRefs(r.Targets)
	if req.TargetsNameNoRelation {
		targets = slotTargets(targets)
	}
	siblings, err := b.siblingBindings(ctx, mergeRefs(sqlOnly, targets), r)
	if err != nil {
		return nil, err
	}
	if len(facts) == 0 {
		return policyBindings(siblings), nil
	}
	refs, err := b.qualifyTargets(ctx, sqlOnly, targets)
	if err != nil {
		return nil, err
	}
	if b.resolver != nil && needsResolution(refs) {
		if refs, err = resolveCatalog(ctx, b.resolver, refs); err != nil {
			return nil, fmt.Errorf("resolve index tables and partition roots: %w", err)
		}
	}
	if refs, err = b.withTableIndexes(ctx, facts, refs, r.SQL); err != nil {
		return nil, err
	}
	return policyBindings(append(Bind(facts, refs, r), siblings...)), nil
}

// siblingBindings binds the request with the sibling databases' confirmed
// slot facts when it touches a slot. An unreadable sibling is an error
// (the gate fails closed): its fact cannot be shown not to bind.
func (b *Binder) siblingBindings(ctx context.Context, refs []ObjectRef, r Request) (
	[]Binding, error) {
	if b.siblings == nil || !touchesSlot(refs) {
		return nil, nil
	}
	var out []Binding
	for _, sib := range b.siblings() {
		all, err := sib.Facts.Confirmed(ctx)
		if err != nil {
			return nil, fmt.Errorf("read confirmed slot facts of sibling database %q: %w",
				sib.Database, err)
		}
		for _, bound := range Bind(slotConsumerFacts(all), refs, r) {
			bound.Database = sib.Database
			out = append(out, bound)
		}
	}
	return out, nil
}

func touchesSlot(refs []ObjectRef) bool {
	for _, r := range refs {
		if r.Kind == KindSlot {
			return true
		}
	}
	return false
}

func slotConsumerFacts(all []Fact) []Fact {
	var out []Fact
	for _, f := range all {
		if f.Type == TypeSlotConsumer {
			out = append(out, f)
		}
	}
	return out
}

// needsResolution reports whether the catalog can complete a ref: a
// relation of unknown kind, an index without its table or partition root,
// or a qualified table without its partition root. Unqualified tables are
// matched as written (any schema).
func needsResolution(refs []ObjectRef) bool {
	for _, r := range refs {
		switch {
		case r.Kind == KindRelation:
		case r.Kind == KindIndex && (r.TableName == "" || r.RootName == ""):
		case r.Kind == KindTable && r.Schema != "" && r.RootName == "":
		default:
			continue
		}
		return true
	}
	return false
}

func policyBindings(bs []Binding) []policy.FactBinding {
	if len(bs) == 0 {
		return nil
	}
	out := make([]policy.FactBinding, 0, len(bs))
	for _, b := range bs {
		pb := policy.FactBinding{FactID: b.Fact.ID, Type: string(b.Fact.Type),
			Subject: b.Fact.Subject, Route: string(b.Route), Summary: b.Fact.Describe(),
			Object: b.Object, ConfirmedBy: b.Fact.DecidedBy, Database: b.Database}
		if b.Fact.DecidedAt != nil {
			pb.ConfirmedAt = *b.Fact.DecidedAt
		}
		out = append(out, pb)
	}
	return out
}
