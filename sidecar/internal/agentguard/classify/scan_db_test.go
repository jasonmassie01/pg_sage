package classify

import (
	"context"
	"errors"
	"testing"
)

// Spec §6.7: the posture cycle detects new columns and proposes classes;
// no grant follows until a human confirms them. Without a model the name
// heuristics alone propose (§6.16 fallback); a failed model call never
// loses the deterministic proposals.

type fakeSuggester struct {
	calls int
	seen  int
	out   func([]Column) []Proposal
	err   error
}

func (f *fakeSuggester) Suggest(_ context.Context, cols []Column) ([]Proposal, error) {
	f.calls++
	f.seen += len(cols)
	if f.err != nil {
		return nil, f.err
	}
	if f.out == nil {
		return nil, nil
	}
	return f.out(cols), nil
}

func scanAll(t *testing.T, ctx context.Context, s *Store, opts ScanOptions) ScanResult {
	t.Helper()
	var total ScanResult
	for range 1000 {
		res, err := Scan(ctx, s, opts)
		if err != nil {
			t.Fatal(err)
		}
		total.Read += res.Read
		total.Proposed += res.Proposed
		total.ModelErr = errors.Join(total.ModelErr, res.ModelErr)
		opts.After = res.Next
		if res.Done {
			total.Done = true
			return total
		}
	}
	t.Fatal("scan never finished")
	return total
}

func TestScan_RulesProposeAndNothingIsConfirmed(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	res := scanAll(t, ctx, s, ScanOptions{Limit: 2})
	if res.Read < 5 || res.Proposed < 2 || !res.Done {
		t.Fatalf("scan: %+v", res)
	}
	pw := resolve(t, ctx, s, sch, "password")
	eff, err := s.ClassOf(ctx, pw.RelID, pw.AttNum)
	if err != nil || eff.Class != ClassSecret || eff.Confirmed {
		t.Fatalf("password proposed secret, unconfirmed: %+v %v", eff, err)
	}
	email := resolve(t, ctx, s, sch, "email")
	if eff, _ := s.ClassOf(ctx, email.RelID, email.AttNum); eff.Class != ClassPII ||
		eff.Confirmed {
		t.Fatalf("email: %+v", eff)
	}
	confirmed, _, err := s.List(ctx, Filter{Status: []Status{StatusConfirmed}})
	if err != nil || len(confirmed) != 0 {
		t.Fatalf("a scan never confirms: %+v %v", confirmed, err)
	}
	again := scanAll(t, ctx, s, ScanOptions{Limit: 50})
	if again.Proposed != 0 {
		t.Fatalf("a second scan re-proposes nothing: %+v", again)
	}
}

func TestScan_ModelAddsAndFailsSoft(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	model := &fakeSuggester{out: func(cols []Column) []Proposal {
		var out []Proposal
		for _, c := range cols {
			if c.Name == "nickname" && c.Schema == sch {
				out = append(out, Proposal{Column: c, Class: ClassPII, Source: SourceModel,
					ProposedBy: ModelProposer, Rationale: "comment says shown publicly",
					Evidence: []Citation{{Kind: "column", Ref: c.QualifiedName()}}})
			}
		}
		return out
	}}
	res := scanAll(t, ctx, s, ScanOptions{Limit: 500, Model: model})
	if res.ModelErr != nil || model.calls == 0 {
		t.Fatalf("model scan: %+v calls %d", res, model.calls)
	}
	nick := resolve(t, ctx, s, sch, "nickname")
	rc, err := s.Lookup(ctx, nick.RelID)
	if err != nil || rc.Columns[nick.AttNum].Source != SourceModel {
		t.Fatalf("model proposal stored: %+v %v", rc.Columns[nick.AttNum], err)
	}

	_, _ = pool.Exec(ctx, "DELETE FROM sage.facts")
	broken := &fakeSuggester{err: errors.New("429 rate limited")}
	res = scanAll(t, ctx, s, ScanOptions{Limit: 500, Model: broken})
	if res.ModelErr == nil || res.Proposed < 2 {
		t.Fatalf("model failure keeps the rule proposals: %+v", res)
	}
	pw := resolve(t, ctx, s, sch, "password")
	if eff, _ := s.ClassOf(ctx, pw.RelID, pw.AttNum); eff.Class != ClassSecret {
		t.Fatalf("rules still applied: %+v", eff)
	}
}

func TestScan_SkipsDecidedColumnsAndSystemSchemas(t *testing.T) {
	pool, ctx, sch := livePool(t)
	s := NewStore(pool)
	if _, err := s.Set(ctx, resolve(t, ctx, s, sch, "password"), ClassClean,
		"a@example.com", "hashed elsewhere"); err != nil {
		t.Fatal(err)
	}
	model := &fakeSuggester{}
	scanAll(t, ctx, s, ScanOptions{Limit: 500, Model: model})
	cols, _, err := s.UnclassifiedColumns(ctx, Cursor{}, 5000)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		switch c.Schema {
		case "sage", "pg_catalog", "information_schema", "pg_toast":
			t.Fatalf("scanned a system column %s", c.QualifiedName())
		}
		if c.Schema == sch && c.Name == "password" {
			t.Fatal("an operator-decided column is not re-read")
		}
	}
	pw := resolve(t, ctx, s, sch, "password")
	if eff, _ := s.ClassOf(ctx, pw.RelID, pw.AttNum); eff.Class != ClassClean ||
		!eff.Confirmed {
		t.Fatalf("a scan never overrides an operator: %+v", eff)
	}
	if _, err := Scan(ctx, NewStore(nil), ScanOptions{}); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store: %v", err)
	}
}
