package principalfile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Principal is a principal as the core store holds it.
type Principal struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Sponsor    string     `json:"sponsor"`
	Profile    string     `json:"profile"`
	EnvCeiling string     `json:"env_ceiling"`
	Tenant     string     `json:"tenant"`
	Status     string     `json:"status"` // active | frozen | retired
	ManagedBy  string     `json:"managed_by"`
	CreatedBy  string     `json:"created_by"`
	Identities []Identity `json:"identities"`
}

// Backend is the principal store (the core workstream implements it).
type Backend interface {
	List(ctx context.Context) ([]Principal, error)
	Create(ctx context.Context, p Principal) (string, error) // returns the id
	Update(ctx context.Context, id string, c Change) error   // one field
	Bind(ctx context.Context, id string, ident Identity) error
	Unbind(ctx context.Context, id string, ident Identity) error
}

// Catalog answers what the file may reference.
type Catalog interface {
	ProfileClasses(profile string) ([]string, bool)
	UserExists(sponsor string) bool
}

// Op is a planned operation.
type Op string

// Operations.
const (
	OpCreate   Op = "create"
	OpAdopt    Op = "adopt"
	OpUpdate   Op = "update"
	OpFreeze   Op = "freeze"
	OpUnfreeze Op = "unfreeze"
	OpRetire   Op = "retire"
	OpBind     Op = "bind"
	OpUnbind   Op = "unbind"
)

// Change is one planned change.
type Change struct {
	Op        Op        `json:"op"`
	Principal string    `json:"principal"`
	Field     string    `json:"field,omitempty"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Identity  *Identity `json:"identity,omitempty"`
	Widening  bool      `json:"widening"`
}

// Plan is what applying a file would change.
type Plan struct {
	File      string   `json:"file"`
	FileHash  string   `json:"file_hash"`
	StateHash string   `json:"state_hash"`
	Changes   []Change `json:"changes"`
	Unmanaged []string `json:"unmanaged"`
	Warnings  []string `json:"warnings"`
}

// Errors.
var (
	ErrConflict = errors.New("principals file: conflicts with the current state")
	errCatalog  = errors.New("principals file: unknown reference")
)

// Widening reports whether any change widens what an agent may do.
func (p *Plan) Widening() bool {
	for _, c := range p.Changes {
		if c.Widening {
			return true
		}
	}
	return false
}

// managedBy is the owner marker of principals this file manages.
func (f *File) managedBy() string { return "file:" + f.Metadata.Name }

// MakePlan compares the file with the backend's state.
func MakePlan(ctx context.Context, f *File, b Backend, cat Catalog) (*Plan, error) {
	if f == nil || b == nil || cat == nil {
		return nil, fmt.Errorf("principals file: plan needs a file, a backend and a catalog")
	}
	state, err := b.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("principals file: read principals: %w", err)
	}
	p := &Plan{File: f.Metadata.Name, FileHash: fileHash(f), StateHash: stateHash(state),
		Unmanaged: []string{}, Warnings: []string{}}
	if err := checkCatalog(f, cat, p); err != nil {
		return nil, err
	}
	byName := map[string]Principal{}
	for _, s := range state {
		byName[s.Name] = s
	}
	for _, want := range f.Spec.Principals {
		changes, err := planPrincipal(f, want, byName)
		if err != nil {
			return nil, err
		}
		p.Changes = append(p.Changes, changes...)
	}
	planRemovals(f, state, p)
	return p, nil
}

func checkCatalog(f *File, cat Catalog, p *Plan) error {
	var errs []error
	for _, want := range f.Spec.Principals {
		if _, ok := cat.ProfileClasses(want.Profile); !ok {
			errs = append(errs, fmt.Errorf("principal %q: profile %q is not in "+
				"agents.profiles", want.Name, want.Profile))
		}
		switch {
		case want.Sponsor == "":
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s is unsponsored: it stays at "+
				"L0 everywhere until a sponsor is set", want.Name))
		case !cat.UserExists(want.Sponsor):
			errs = append(errs, fmt.Errorf("principal %q: sponsor %q is not a pg_sage user",
				want.Name, want.Sponsor))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", errCatalog, errors.Join(errs...))
	}
	return nil
}

// planPrincipal plans one declared principal.
func planPrincipal(f *File, want PrincipalSpec, byName map[string]Principal) (
	[]Change, error) {
	have, exists := byName[want.Name]
	if !exists {
		out := []Change{{Op: OpCreate, Principal: want.Name, Widening: true}}
		for i := range want.Identities {
			out = append(out, Change{Op: OpBind, Principal: want.Name,
				Identity: &want.Identities[i], Widening: true})
		}
		return out, nil
	}
	var out []Change
	switch {
	case have.Status == "retired":
		return nil, fmt.Errorf("%w: %s is retired; declare a new name", ErrConflict,
			want.Name)
	case have.ManagedBy == "":
		out = append(out, Change{Op: OpAdopt, Principal: want.Name, Field: "managed_by",
			To: f.managedBy()})
	case have.ManagedBy != f.managedBy():
		return nil, fmt.Errorf("%w: %s is managed by %s", ErrConflict, want.Name,
			have.ManagedBy)
	}
	if have.Tenant != want.Tenant {
		return nil, fmt.Errorf("%w: %s belongs to tenant %q; a tenant cannot change, "+
			"declare a new principal", ErrConflict, want.Name, have.Tenant)
	}
	out = append(out, fieldChanges(want, have)...)
	return append(out, identityChanges(want, have)...), nil
}

func fieldChanges(want PrincipalSpec, have Principal) []Change {
	var out []Change
	upd := func(field, from, to string, widening bool) {
		if from != to {
			out = append(out, Change{Op: OpUpdate, Principal: want.Name, Field: field,
				From: from, To: to, Widening: widening})
		}
	}
	// Widening follows core's Patch.Widens: any other profile, or a higher
	// ceiling; a sponsor change does not widen.
	upd("profile", have.Profile, want.Profile, true)
	upd("env_ceiling", have.EnvCeiling, want.EnvCeiling,
		envRank[want.EnvCeiling] > envRank[have.EnvCeiling])
	upd("sponsor", have.Sponsor, want.Sponsor, false)
	if have.Status != want.Status {
		op, widening := OpFreeze, false
		if want.Status == "active" {
			op, widening = OpUnfreeze, true
		}
		out = append(out, Change{Op: op, Principal: want.Name, Field: "status",
			From: have.Status, To: want.Status, Widening: widening})
	}
	return out
}

func identityChanges(want PrincipalSpec, have Principal) []Change {
	wanted, held := map[Identity]bool{}, map[Identity]bool{}
	for _, id := range want.Identities {
		wanted[id] = true
	}
	var out []Change
	for _, id := range have.Identities {
		held[id] = true
	}
	for i, id := range want.Identities {
		if !held[id] {
			out = append(out, Change{Op: OpBind, Principal: want.Name,
				Identity: &want.Identities[i], Widening: true})
		}
	}
	for i, id := range have.Identities {
		if !wanted[id] {
			out = append(out, Change{Op: OpUnbind, Principal: want.Name,
				Identity: &have.Identities[i]})
		}
	}
	return out
}

// planRemovals retires (under prune) what the file created and no longer
// lists, and names principals it does not manage.
func planRemovals(f *File, state []Principal, p *Plan) {
	listed := map[string]bool{}
	for _, want := range f.Spec.Principals {
		listed[want.Name] = true
	}
	for _, s := range state {
		switch {
		case listed[s.Name] || s.Status == "retired":
		case s.ManagedBy != f.managedBy():
			p.Unmanaged = append(p.Unmanaged, s.Name)
		case f.Spec.Prune:
			p.Changes = append(p.Changes, Change{Op: OpRetire, Principal: s.Name,
				Field: "status", From: s.Status, To: "retired"})
		default:
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s is no longer listed; set "+
				"spec.prune to retire it", s.Name))
		}
	}
	sort.Strings(p.Unmanaged)
}

// Render prints the plan: + adds, ~ changes, - narrows or removes, and !
// marks a widening change that needs approval.
func (p *Plan) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan for %s: %d change(s)\n", p.File, len(p.Changes))
	for _, c := range p.Changes {
		mark := "~"
		switch c.Op {
		case OpCreate, OpBind:
			mark = "+"
		case OpRetire, OpUnbind, OpFreeze:
			mark = "-"
		}
		line := fmt.Sprintf("%s %s %s", mark, c.Op, c.Principal)
		if c.Field != "" {
			line += fmt.Sprintf(" %s: %q -> %q", c.Field, c.From, c.To)
		}
		if c.Identity != nil {
			line += " " + c.Identity.Issuer + " " + c.Identity.Subject
		}
		if c.Widening {
			line += "  ! widening"
		}
		b.WriteString(line + "\n")
	}
	for _, w := range p.Warnings {
		b.WriteString("warning: " + w + "\n")
	}
	return b.String()
}
