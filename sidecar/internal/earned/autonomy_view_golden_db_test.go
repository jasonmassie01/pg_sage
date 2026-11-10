package earned

import (
	"bytes"
	"encoding/json"
	"testing"
)

// earned-08 golden: the batched Autonomy view is byte-identical to the
// view built the way it was before, one pair's Granted and Evidence at a
// time, on a seeded mixed ledger (the JSON of /api/v1/sre/autonomy must
// not change).

// perPairView builds the view from the per-pair reads.
func perPairView(t *testing.T, f *fixture) View {
	t.Helper()
	svc := f.svc
	pending, err := svc.PendingProposals(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	byPair := map[pairKey]*Proposal{}
	for i := range pending {
		byPair[pairKey{pending[i].Family, pending[i].Class}] = &pending[i]
	}
	v := View{GeneratedAt: svc.now(), Thresholds: svc.cfg.Thresholds, GameDays: []EvalRun{}}
	for _, fam := range Families() {
		fv := FamilyView{Family: fam, Classes: []ClassView{}}
		for _, c := range ApplicableClasses(fam) {
			ev, err := svc.Evidence(f.ctx, fam, c)
			if err != nil {
				t.Fatal(err)
			}
			st, err := svc.Granted(f.ctx, fam, c)
			if err != nil {
				t.Fatal(err)
			}
			fv.Shadow, fv.Bench = ev.Shadow, SummarizeBench(ev.Bench)
			fv.Classes = append(fv.Classes, svc.classView(st, ev, byPair[pairKey{fam, c}]))
		}
		v.Families = append(v.Families, fv)
	}
	if v.Bench, err = svc.store.LatestBench(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	runs, err := svc.store.GameDayRuns(f.ctx, svc.now().Add(-svc.cfg.Thresholds.BenchMaxAge))
	if err != nil {
		t.Fatal(err)
	}
	if runs != nil {
		v.GameDays = runs
	}
	return v
}

func TestAutonomyViewJSONMatchesThePerPairReads(t *testing.T) {
	lf := newLimiterFixture(t)
	lf.seedL3()
	seedMixedLedger(t, lf.fixture)
	if _, err := lf.svc.ProposePromotions(lf.ctx); err != nil {
		t.Fatal(err)
	}
	for name, annotate := range map[string]bool{"plain": false, "annotated": true} {
		got, err := lf.svc.View(lf.ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := perPairView(t, lf.fixture)
		if annotate {
			lf.lim.Annotate(lf.ctx, &got)
			lf.lim.Annotate(lf.ctx, &want)
		}
		gotJSON, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("%s view JSON differs from the per-pair reads:\n got %s\nwant %s", name,
				gotJSON, wantJSON)
		}
		// Grandfathered levels are self-initiated only, so the Autonomy
		// page (incident families) shows carried-over and ledger rows.
		if !bytes.Contains(gotJSON, []byte(`"provenance":"carried_over"`)) ||
			!bytes.Contains(gotJSON, []byte(`"provenance":"ledger"`)) {
			t.Fatalf("%s fixture lost its mixed provenances: %s", name, gotJSON)
		}
	}
}
