package principalfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Apply errors.
var (
	ErrStalePlan           = errors.New("principals file: the file or the principals changed since the plan; plan again")
	ErrWideningNotApproved = errors.New("principals file: the plan widens access; approve it explicitly")
)

// ApplyOptions are the human decisions apply needs.
type ApplyOptions struct {
	Actor string // who applies (recorded as created_by)
	// ApproveWidening is the explicit approval of a widening plan; every
	// grant is operator-approved (G1).
	ApproveWidening bool
}

// Result is what apply did.
type Result struct {
	Applied []Change `json:"applied"`
	Failed  *Change  `json:"failed,omitempty"`
}

// Apply executes p after checking it still matches f and the backend's
// state, narrowing changes first. It stops at the first failure; planning
// again shows what is left.
func Apply(ctx context.Context, p *Plan, f *File, b Backend, opts ApplyOptions) (Result,
	error) {
	var res Result
	if p == nil || f == nil || b == nil || opts.Actor == "" {
		return res, fmt.Errorf("principals file: apply needs a plan, a file, a backend " +
			"and an actor")
	}
	state, err := b.List(ctx)
	if err != nil {
		return res, fmt.Errorf("principals file: read principals: %w", err)
	}
	if fileHash(f) != p.FileHash || stateHash(state) != p.StateHash {
		return res, ErrStalePlan
	}
	if p.Widening() && !opts.ApproveWidening {
		return res, ErrWideningNotApproved
	}
	ids := map[string]string{}
	for _, s := range state {
		ids[s.Name] = s.ID
	}
	for _, c := range ordered(p.Changes) {
		if err := applyOne(ctx, c, f, b, ids, opts.Actor); err != nil {
			failed := c
			res.Failed = &failed
			return res, fmt.Errorf("principals file: %s %s: %w", c.Op, c.Principal, err)
		}
		res.Applied = append(res.Applied, c)
	}
	return res, nil
}

// ordered puts narrowing changes before widening ones, keeping plan order
// within each group (a create precedes its binds).
func ordered(changes []Change) []Change {
	out := append([]Change(nil), changes...)
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Widening && out[j].Widening })
	return out
}

func applyOne(ctx context.Context, c Change, f *File, b Backend, ids map[string]string,
	actor string) error {
	switch c.Op {
	case OpCreate:
		want := find(f, c.Principal)
		id, err := b.Create(ctx, Principal{Name: want.Name, Sponsor: want.Sponsor,
			Profile: want.Profile, EnvCeiling: want.EnvCeiling, Tenant: want.Tenant,
			Status: want.Status, ManagedBy: f.managedBy(), CreatedBy: actor})
		ids[c.Principal] = id
		return err
	case OpBind:
		return b.Bind(ctx, ids[c.Principal], *c.Identity)
	case OpUnbind:
		return b.Unbind(ctx, ids[c.Principal], *c.Identity)
	default:
		return b.Update(ctx, ids[c.Principal], c)
	}
}

func find(f *File, name string) PrincipalSpec {
	for _, p := range f.Spec.Principals {
		if p.Name == name {
			return p
		}
	}
	return PrincipalSpec{Name: name}
}

func fileHash(f *File) string { return digest(f) }

// stateHash digests the principals in a canonical order.
func stateHash(state []Principal) string {
	cp := make([]Principal, len(state))
	for i, s := range state {
		cp[i] = s
		cp[i].Identities = append([]Identity(nil), s.Identities...)
		sort.Slice(cp[i].Identities, func(a, b int) bool {
			x, y := cp[i].Identities[a], cp[i].Identities[b]
			return x.Issuer+"|"+x.Subject < y.Issuer+"|"+y.Subject
		})
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i].Name < cp[j].Name })
	return digest(cp)
}

func digest(v any) string {
	body, _ := json.Marshal(v)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
