package clone

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Kind is what a provider's clone is (lab-05a): it sets how long a clone
// takes, what it costs, and whether it needs an opt-in.
type Kind string

const (
	// KindThinClone is a copy-on-write clone on an existing host (DBLab).
	KindThinClone Kind = "thin_clone"
	// KindBranch is a storage branch with its own compute (Lakebase, Neon).
	KindBranch Kind = "branch"
	// KindCopyOnWrite is a copy-on-write cluster clone (Aurora).
	KindCopyOnWrite Kind = "copy_on_write"
	// KindFullCopy restores a whole instance from a snapshot (RDS, Cloud
	// SQL, AlloyDB, Azure): hours and a full instance bill.
	KindFullCopy Kind = "full_copy"
	// KindSchemaOnly holds no data: never accepted for a rehearsal.
	KindSchemaOnly Kind = "schema_only"
)

// Capability is what a provider says of its clones.
type Capability struct {
	Kind Kind
	// TypicalReady is how long a create usually takes.
	TypicalReady time.Duration
	// MaxTTL is the longest a clone may live on the provider; 0: no
	// provider-side bound.
	MaxTTL time.Duration
}

// Describer is a provider that says what its clones are. A provider
// without it is treated as a full copy (fail closed: it needs the opt-in).
type Describer interface {
	Describe(context.Context) (Capability, error)
}

// DescribeOf is p's capability, a full copy when p does not describe
// itself.
func DescribeOf(ctx context.Context, p Provider) (Capability, error) {
	if p == nil {
		return Capability{}, errors.New("clone: describe: provider is required")
	}
	d, ok := p.(Describer)
	if !ok {
		return Capability{Kind: KindFullCopy}, nil
	}
	c, err := d.Describe(ctx)
	if err != nil {
		return Capability{}, fmt.Errorf("describe clone provider: %w", err)
	}
	return c, nil
}

// Opt-in errors.
var (
	ErrFullCopyNotAllowed = errors.New("clone: a full-copy clone needs " +
		"clone.allow_full_copy: true")
	ErrCloneKindRefused = errors.New("clone: this clone kind is never used for a rehearsal")
)

// CheckOptIn is p's capability when a clone of it may be made: a full
// copy only with allowFullCopy, a schema-only or unknown kind never.
func CheckOptIn(ctx context.Context, p Provider, allowFullCopy bool) (Capability, error) {
	c, err := DescribeOf(ctx, p)
	if err != nil {
		return Capability{}, err
	}
	switch c.Kind {
	case KindThinClone, KindBranch, KindCopyOnWrite:
		return c, nil
	case KindFullCopy:
		if !allowFullCopy {
			return Capability{}, fmt.Errorf("%w: it restores a whole instance (hours, a "+
				"full instance bill); nothing was created", ErrFullCopyNotAllowed)
		}
		return c, nil
	default:
		return Capability{}, fmt.Errorf("%w: kind %q holds no copy of the data or is "+
			"unknown; nothing was created", ErrCloneKindRefused, c.Kind)
	}
}

// Rate is the operator's price for the source of a clone (cost.rates;
// pg_sage ships no price tables).
type Rate struct {
	PerHour  float64
	Currency string
	AsOf     string // the date the operator read the price
	Source   string // the cost.rates entry, by resource or database
}

func (r *Rate) usable() bool {
	return r != nil && r.Currency != "" && r.PerHour >= 0 && !math.IsNaN(r.PerHour) &&
		!math.IsInf(r.PerHour, 0)
}

// Cost is a clone's cost label, written to its receipt before the
// provider is asked for it.
type Cost struct {
	Kind          Kind    `json:"kind"`
	InstanceHours float64 `json:"instance_hours"`
	Priced        bool    `json:"priced"`
	Amount        float64 `json:"amount,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	Source        string  `json:"source,omitempty"`
	AsOf          string  `json:"as_of,omitempty"`
	Label         string  `json:"label"`
}

// Estimate is the cost of holding one clone for up to hold. A thin clone
// runs on the existing host and adds no instance; every other kind adds
// one instance for the hold. Without a usable rate it is unpriced.
func (c Capability) Estimate(hold time.Duration, rate *Rate) Cost {
	hours := 0.0
	if hold > 0 && c.Kind != KindThinClone {
		hours = round2(hold.Hours())
	}
	out := Cost{Kind: c.Kind, InstanceHours: hours}
	if !rate.usable() {
		out.Label = unpricedLabel(c.Kind, hours)
		return out
	}
	out.Priced, out.Amount = true, round2(hours*rate.PerHour)
	out.Currency, out.Source, out.AsOf = rate.Currency, rate.Source, rate.AsOf
	out.Label = fmt.Sprintf("up to %.2f %s for %.2f instance-hours (%s; cost.rates %s "+
		"as of %s)", out.Amount, out.Currency, hours, c.Kind, rate.Source, rate.AsOf)
	return out
}

func unpricedLabel(kind Kind, hours float64) string {
	if kind == KindThinClone {
		return "unpriced: thin clone on the existing host, 0.00 new instance-hours"
	}
	return fmt.Sprintf("unpriced: up to %.2f instance-hours (%s; no cost.rates entry "+
		"prices the source)", hours, kind)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
