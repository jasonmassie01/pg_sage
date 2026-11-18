package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/policy"
)

// setPlan is one set's planned layout step and document step, with the
// document it would have in force afterwards.
type setPlan struct {
	layout    *step // create, update or adopt
	doc       *step // ratify or propose
	waiting   *Change
	effective *policy.Policy
}

// planSets plans every set file and retires the config-owned sets no file
// names. A layout change (create, selectors, priority, retire) that would
// widen the document in force on any database is held: written by no
// file, it waits for a person to make it in pg_sage.
func (p *planner) planSets(ctx context.Context, tx pgx.Tx) error {
	next := map[string]policy.SetDocument{}
	for _, s := range p.st.setList {
		next[s.Set.Name] = s
	}
	var layout, docs []step
	for _, spec := range p.dir.Sets {
		sp, ok := p.planSet(ctx, tx, spec)
		if !ok {
			continue
		}
		if sp.layout != nil {
			layout = append(layout, *sp.layout)
		}
		if sp.doc != nil {
			docs = append(docs, *sp.doc)
		}
		if sp.waiting != nil {
			p.note(*sp.waiting)
		}
		next[spec.File.Name] = policy.SetDocument{Set: spec.File.Set(), Policy: sp.effective}
	}
	// Retires run first, so a rename (a retire plus a create at the same
	// priority) never ties with itself.
	layout = append(p.retireSets(next), layout...)
	if len(p.problems) > 0 || len(layout) == 0 {
		p.steps = append(p.steps, docs...)
		return nil
	}
	widened, err := policy.WideningMembersTx(ctx, tx, sortedSets(next), p.now)
	if err != nil {
		return fmt.Errorf("gitops: judge the policy-set layout: %w", err)
	}
	if len(widened) == 0 {
		p.steps = append(p.steps, layout...)
		p.steps = append(p.steps, docs...)
		return nil
	}
	p.holdLayout(layout, widened)
	p.keepOwnedDocuments(docs)
	return nil
}

// keepOwnedDocuments plans the document steps of a held layout: only a
// set the file already owns keeps its step. A new set was not created,
// and an API set's adopt was held, so its document stays the API's.
func (p *planner) keepOwnedDocuments(docs []step) {
	for _, d := range docs {
		if cur, exists := p.st.sets[d.change.Name]; exists &&
			cur.ManagedBy == policy.ManagedByConfig {
			p.steps = append(p.steps, d)
			continue
		}
		c := d.change
		c.Detail, c.Op = c.Op+" held with its set's layout change", OpHold
		p.note(c)
	}
}

func sortedSets(m map[string]policy.SetDocument) []policy.SetDocument {
	out := make([]policy.SetDocument, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Set.Name < out[j].Set.Name })
	return out
}

// holdLayout reports every layout step as held, naming the databases.
func (p *planner) holdLayout(layout []step, widened []policy.Member) {
	names := make([]string, 0, len(widened))
	for _, m := range widened {
		names = append(names, m.Name)
	}
	detail := fmt.Sprintf("held: this %s would widen the policy in force on %s; make it "+
		"in pg_sage as a person, or narrow those databases' documents first",
		"set layout change", strings.Join(names, ", "))
	for _, s := range layout {
		c := s.change
		c.Detail = c.Op + " " + detail
		c.Op = OpHold
		p.note(c)
	}
}

// planSet plans one set file against its stored row.
func (p *planner) planSet(ctx context.Context, tx pgx.Tx, spec SetSpec) (setPlan, bool) {
	name := spec.File.Name
	cur, exists := p.st.sets[name]
	if exists && cur.ManagedBy != policy.ManagedByConfig && !spec.Adopt {
		p.problem(spec.problem("name", "policy set %s is owned by the API; add adopt: true "+
			"to this file to manage it here", name))
		return setPlan{}, false
	}
	change := Change{Kind: KindPolicySet, Name: name, File: spec.Path}
	put := func(ctx context.Context, tx pgx.Tx) error {
		_, err := policy.PutSetTx(ctx, tx, policy.PutSetRequest{Set: spec.File.Set(),
			Actor: spec.Actor(), Dimensions: rubricDimensions()}, policy.ManagedByConfig)
		return err
	}
	var sp setPlan
	base := cur.Policy
	switch {
	case !exists:
		base = p.st.sets[policy.DefaultSetName].Policy
		if base == nil {
			p.problem(spec.problem("name", "the default policy set has no document yet; "+
				"start pg_sage once so it bootstraps one"))
			return setPlan{}, false
		}
		change.Op = OpCreate
		sp.layout = &step{change: change, apply: put}
	case cur.ManagedBy != policy.ManagedByConfig:
		change.Op = OpAdopt
		sp.layout = &step{change: change, apply: put}
	case !sameLayout(cur.Set, spec.File.Set()):
		change.Op = OpUpdate
		sp.layout = &step{change: change, apply: put}
	}
	sp.effective = base
	return p.planSetDocument(ctx, tx, spec, base, sp)
}

func sameLayout(a, b policy.PolicySet) bool {
	return a.Priority == b.Priority && jsonEqual(a.Selectors, b.Selectors) &&
		jsonEqual(a.Requirements, b.Requirements)
}

func jsonEqual(a, b any) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	var va, vb any
	return json.Unmarshal(ra, &va) == nil && json.Unmarshal(rb, &vb) == nil &&
		reflect.DeepEqual(normalizeEmpty(va), normalizeEmpty(vb))
}

// normalizeEmpty treats a null list as an empty one.
func normalizeEmpty(v any) any {
	if v == nil {
		return []any{}
	}
	return v
}

// planSetDocument plans the file's document against base, the document in
// force once the layout step ran: equal is nothing, narrower is ratified
// as the file, wider is proposed (or already waiting).
func (p *planner) planSetDocument(ctx context.Context, tx pgx.Tx, spec SetSpec,
	base *policy.Policy, sp setPlan) (setPlan, bool) {
	doc := spec.File.Document
	if doc == nil {
		return sp, true
	}
	diff, err := policy.DocumentDiff(base.Document, doc.Body)
	if err != nil {
		p.problem(spec.problem("document", "document: %v", err))
		return setPlan{}, false
	}
	if len(diff) == 0 && base.Profile == doc.Profile {
		return sp, true
	}
	widens, err := policy.WidensJSON(base.Document, doc.Body)
	if err != nil {
		p.problem(spec.problem("document", "document: %v", err))
		return setPlan{}, false
	}
	change := Change{Kind: KindPolicySet, Name: spec.File.Name, File: spec.Path,
		Detail: fmt.Sprintf("%d document fields differ", len(diff))}
	if !widens {
		change.Op = OpRatify
		sp.doc = &step{change: change, apply: ratifySetDocument(spec)}
		sp.effective = &policy.Policy{Profile: doc.Profile, Document: doc.Body}
		return sp, true
	}
	if waiting, err := p.proposalWaits(ctx, tx, spec); err != nil || waiting {
		if err != nil {
			p.problem(spec.problem("document", "reading pending proposals: %v", err))
			return setPlan{}, false
		}
		change.Op, change.Detail = OpWaiting, "a wider document waits for a person's ratify"
		sp.waiting = &change
		return sp, true
	}
	change.Op = OpPropose
	change.Detail = "a wider document is proposed; a person ratifies it in pg_sage"
	sp.doc = &step{change: change, apply: proposeSetDocument(spec)}
	return sp, true
}

// proposalWaits reports whether an open proposal of the set is the file's
// document already.
func (p *planner) proposalWaits(ctx context.Context, tx pgx.Tx, spec SetSpec) (bool, error) {
	if _, exists := p.st.sets[spec.File.Name]; !exists {
		return false, nil
	}
	pending, err := policy.PendingSetProposalsTx(ctx, tx, spec.File.Name)
	if err != nil {
		return false, err
	}
	for _, pr := range pending {
		diff, err := policy.DocumentDiff(pr.Document, spec.File.Document.Body)
		if err == nil && len(diff) == 0 && pr.Profile == spec.File.Document.Profile {
			return true, nil
		}
	}
	return false, nil
}

func proposeSetDocument(spec SetSpec) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := policy.ProposeSetTx(ctx, tx, spec.File.Name, spec.File.Document.Profile,
			spec.File.Document.Body, spec.Actor())
		return err
	}
}

// ratifySetDocument files the narrower document and ratifies it with the
// file as actor (an admin-owned file is operator authority).
func ratifySetDocument(spec SetSpec) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		proposal, err := policy.ProposeSetTx(ctx, tx, spec.File.Name,
			spec.File.Document.Profile, spec.File.Document.Body, spec.Actor())
		if err != nil {
			return err
		}
		_, err = policy.RatifyTx(ctx, tx, policy.RatifyRequest{ProposalID: proposal.ID,
			ExpectedVersion: proposal.BaseVersion, Actor: spec.Actor()}, proposal.CreatedAt)
		return err
	}
}

// retireSets plans the removal of config-owned sets no file names.
func (p *planner) retireSets(next map[string]policy.SetDocument) []step {
	named := map[string]bool{}
	for _, spec := range p.dir.Sets {
		named[spec.File.Name] = true
	}
	var out []step
	for _, s := range p.st.setList {
		if s.ManagedBy != policy.ManagedByConfig || named[s.Set.Name] {
			continue
		}
		name, actor := s.Set.Name, "config:"+p.dir.Root+"@"+p.dir.Hash
		delete(next, name)
		out = append(out, step{change: Change{Kind: KindPolicySet, Name: name, Op: OpRetire,
			Detail: "no file names it"},
			apply: func(ctx context.Context, tx pgx.Tx) error {
				return policy.DeleteSetTx(ctx, tx, name, actor)
			}})
	}
	return out
}
