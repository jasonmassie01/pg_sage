package principalfile

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// No concurrent-access tests: MakePlan is a pure function of its inputs,
// and Apply's only shared state is the backend, guarded by the state hash
// (TestApplyRefusesAStalePlan).

func mustParse(t *testing.T, body string) *File {
	t.Helper()
	f, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

func plan(t *testing.T, f *File, b Backend) *Plan {
	t.Helper()
	p, err := MakePlan(context.Background(), f, b, memCatalog{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return p
}

func ops(p *Plan) string {
	var out []string
	for _, c := range p.Changes {
		s := string(c.Op) + " " + c.Principal
		if c.Field != "" {
			s += " " + c.Field
		}
		if c.Identity != nil {
			s += " " + c.Identity.Subject
		}
		if c.Widening {
			s += " (widening)"
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

const managed = "file:prod-agents"

// On an empty install the file creates its principals and binds their
// identities; all of it widens.
func TestPlanCreatesOnEmptyState(t *testing.T) {
	p := plan(t, mustParse(t, validFile), newMem())
	got := ops(p)
	want := "create ci-bot (widening); bind ci-bot 0oa1 (widening); create etl-writer " +
		"(widening)"
	if got != want {
		t.Fatalf("plan = %s\nwant   %s", got, want)
	}
	if !p.Widening() || p.FileHash == "" || p.StateHash == "" {
		t.Fatalf("plan = %+v", p)
	}
	text := p.Render()
	if !strings.Contains(text, "+ create ci-bot") || !strings.Contains(text, "!") {
		t.Fatalf("render = %s", text)
	}
}

// A state that matches the file plans nothing.
func TestPlanIsEmptyWhenConverged(t *testing.T) {
	b := newMem(Principal{Name: "ci-bot", Sponsor: "alice@example.com",
		Profile: "readonly-analyst", EnvCeiling: "stage", Status: "active",
		ManagedBy: managed, Identities: []Identity{{"https://idp.example.com", "0oa1"}}},
		Principal{Name: "etl-writer", Sponsor: "bob@example.com", Profile: "app-writer",
			EnvCeiling: "prod", Status: "frozen", ManagedBy: managed})
	if p := plan(t, mustParse(t, validFile), b); len(p.Changes) != 0 {
		t.Fatalf("converged plan = %s", ops(p))
	}
}

// Widening is judged per field: a higher environment, a profile with
// classes the old lacks, an unfreeze, a new identity; their opposites
// narrow.
func TestPlanClassifiesWidening(t *testing.T) {
	b := newMem(Principal{Name: "ci-bot", Sponsor: "alice@example.com",
		Profile: "app-writer", EnvCeiling: "dev", Status: "frozen", ManagedBy: managed,
		Identities: []Identity{{"https://idp.example.com", "old"}}},
		Principal{Name: "etl-writer", Sponsor: "bob@example.com", Profile: "app-writer",
			EnvCeiling: "prod", Status: "frozen", ManagedBy: managed})
	got := ops(plan(t, mustParse(t, validFile), b))
	for _, want := range []string{
		"update ci-bot profile",                // app-writer -> readonly: narrower
		"update ci-bot env_ceiling (widening)", // dev -> stage
		"unfreeze ci-bot status (widening)",
		"bind ci-bot 0oa1 (widening)",
		"unbind ci-bot old",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plan lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "update ci-bot profile (widening)") ||
		strings.Contains(got, "unbind ci-bot old (widening)") {
		t.Fatalf("a narrowing change is marked widening: %s", got)
	}
}

// Principals the file no longer lists are retired only under prune;
// principals someone else manages are never touched.
func TestPlanPruneAndOwnership(t *testing.T) {
	gone := Principal{Name: "old-bot", Profile: "legacy", EnvCeiling: "dev",
		Status: "active", ManagedBy: managed}
	theirs := Principal{Name: "ui-bot", Profile: "legacy", EnvCeiling: "dev",
		Status: "active", ManagedBy: "ui"}
	f := mustParse(t, validFile)
	got := ops(plan(t, f, newMem(gone, theirs)))
	if !strings.Contains(got, "retire old-bot") || strings.Contains(got, "ui-bot") {
		t.Fatalf("prune plan = %s", got)
	}
	f.Spec.Prune = false
	p := plan(t, f, newMem(gone, theirs))
	if strings.Contains(ops(p), "retire") || len(p.Warnings) == 0 ||
		!strings.Contains(strings.Join(p.Warnings, " "), "old-bot") {
		t.Fatalf("no-prune plan = %s warnings %v", ops(p), p.Warnings)
	}
	if strings.Join(p.Unmanaged, ",") != "ui-bot" {
		t.Fatalf("unmanaged = %v", p.Unmanaged)
	}
}

// An existing principal of the same name is adopted when nobody manages it,
// and a conflict when another file or the UI does; a retired one cannot
// return.
func TestPlanAdoptionAndConflicts(t *testing.T) {
	f := mustParse(t, validFile)
	adopt := newMem(Principal{Name: "ci-bot", Sponsor: "alice@example.com",
		Profile: "readonly-analyst", EnvCeiling: "stage", Status: "active"})
	if got := ops(plan(t, f, adopt)); !strings.Contains(got, "adopt ci-bot managed_by") {
		t.Fatalf("adoption plan = %s", got)
	}
	ctx := context.Background()
	other := newMem(Principal{Name: "ci-bot", Profile: "legacy", EnvCeiling: "dev",
		Status: "active", ManagedBy: "file:other"})
	if _, err := MakePlan(ctx, f, other, memCatalog{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("owned elsewhere: err = %v", err)
	}
	retired := newMem(Principal{Name: "ci-bot", Profile: "legacy", EnvCeiling: "dev",
		Status: "retired", ManagedBy: managed})
	if _, err := MakePlan(ctx, f, retired, memCatalog{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired listed: err = %v", err)
	}
}

// Profiles and sponsors must exist; an empty sponsor is allowed with a
// warning (an unsponsored principal stays at L0).
func TestPlanChecksCatalog(t *testing.T) {
	ctx := context.Background()
	f := mustParse(t, validFile)
	f.Spec.Principals[0].Profile = "god-mode"
	if _, err := MakePlan(ctx, f, newMem(), memCatalog{}); err == nil ||
		!strings.Contains(err.Error(), "god-mode") {
		t.Fatalf("unknown profile: err = %v", err)
	}
	f = mustParse(t, validFile)
	f.Spec.Principals[0].Sponsor = "mallory@example.com"
	if _, err := MakePlan(ctx, f, newMem(), memCatalog{}); err == nil ||
		!strings.Contains(err.Error(), "mallory") {
		t.Fatalf("unknown sponsor: err = %v", err)
	}
	f.Spec.Principals[0].Sponsor = ""
	p, err := MakePlan(ctx, f, newMem(), memCatalog{})
	if err != nil || !strings.Contains(strings.Join(p.Warnings, " "), "unsponsored") {
		t.Fatalf("empty sponsor: %v warnings %v", err, p.Warnings)
	}
	if _, err := MakePlan(ctx, nil, newMem(), memCatalog{}); err == nil {
		t.Fatalf("nil file accepted")
	}
}

// Apply refuses widening without explicit approval, and a plan made
// against a state that has since changed; neither applies anything.
func TestApplyGates(t *testing.T) {
	ctx := context.Background()
	f := mustParse(t, validFile)
	b := newMem()
	p := plan(t, f, b)
	if _, err := Apply(ctx, p, f, b, ApplyOptions{Actor: "alice"}); !errors.Is(err,
		ErrWideningNotApproved) || len(b.calls) != 0 {
		t.Fatalf("unapproved widening: err = %v calls %v", err, b.calls)
	}
	if _, err := Apply(ctx, p, f, b, ApplyOptions{}); err == nil {
		t.Fatalf("apply without an actor accepted")
	}
}

func TestApplyRefusesAStalePlan(t *testing.T) {
	ctx := context.Background()
	f := mustParse(t, validFile)
	b := newMem()
	p := plan(t, f, b)
	if _, err := b.Create(ctx, Principal{Name: "sneaky", Profile: "legacy",
		EnvCeiling: "dev", Status: "active"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	b.calls = nil
	_, err := Apply(ctx, p, f, b, ApplyOptions{Actor: "alice", ApproveWidening: true})
	if !errors.Is(err, ErrStalePlan) || len(b.calls) != 0 {
		t.Fatalf("stale state: err = %v calls %v", err, b.calls)
	}
	f2 := mustParse(t, validFile)
	f2.Spec.Principals[0].EnvCeiling = "prod"
	p = plan(t, f, newMem())
	if _, err := Apply(ctx, p, f2, newMem(), ApplyOptions{Actor: "alice",
		ApproveWidening: true}); !errors.Is(err, ErrStalePlan) {
		t.Fatalf("edited file: err = %v", err)
	}
}

// An approved plan applies narrowing changes before widening ones, and a
// fresh plan afterwards is empty (convergence).
func TestApplyConverges(t *testing.T) {
	ctx := context.Background()
	f := mustParse(t, validFile)
	b := newMem(Principal{Name: "old-bot", Profile: "legacy", EnvCeiling: "dev",
		Status: "active", ManagedBy: managed})
	p := plan(t, f, b)
	res, err := Apply(ctx, p, f, b, ApplyOptions{Actor: "alice", ApproveWidening: true})
	if err != nil || len(res.Applied) != len(p.Changes) {
		t.Fatalf("apply: %d of %d, %v", len(res.Applied), len(p.Changes), err)
	}
	if b.calls[0] != "retire old-bot status" {
		t.Fatalf("narrowing was not first: %v", b.calls)
	}
	if again := plan(t, f, b); len(again.Changes) != 0 {
		t.Fatalf("not converged: %s", ops(again))
	}
	for _, pr := range mustList(t, b) {
		if pr.Name == "ci-bot" && (pr.ManagedBy != managed || pr.CreatedBy != "alice") {
			t.Fatalf("ci-bot provenance = %+v", pr)
		}
	}
}

// A backend failure stops apply and reports what was done; re-planning
// shows exactly what is left.
func TestApplyPartialFailure(t *testing.T) {
	ctx := context.Background()
	f := mustParse(t, validFile)
	b := newMem()
	b.failAfter = 1
	p := plan(t, f, b)
	res, err := Apply(ctx, p, f, b, ApplyOptions{Actor: "alice", ApproveWidening: true})
	if !errors.Is(err, errInjected) || len(res.Applied) != 1 || res.Failed == nil {
		t.Fatalf("partial: applied %d failed %+v err %v", len(res.Applied), res.Failed, err)
	}
	b.failAfter = 0
	left := plan(t, f, b)
	if len(left.Changes) != len(p.Changes)-1 {
		t.Fatalf("left after partial = %s", ops(left))
	}
}

func mustList(t *testing.T, b Backend) []Principal {
	t.Helper()
	ps, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return ps
}
