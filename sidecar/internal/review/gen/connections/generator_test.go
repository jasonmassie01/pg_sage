package connections

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

// Nil/empty: no session samples (and nothing window-wide) is coverage
// missing connections and no candidates.
func TestBuild_NoSessionSamplesIsMissingConnections(t *testing.T) {
	cands, cov := build(t, plainPack(t), Inputs{})
	if len(cands) != 0 || cov.Status != candidate.CoveragePartial ||
		!slices.Contains(cov.Missing, "connections") {
		t.Fatalf("empty inputs: %+v %+v", cands, cov)
	}
}

// A pack without its connections section cannot cite a session fact, so
// nothing is proposed and the section is missing.
func TestBuild_PackWithoutConnectionsSectionIsMissing(t *testing.T) {
	p, err := reviewtest.Build(reviewtest.Spec{Database: "db1",
		Window: reviewtest.Window(90)})
	if err != nil {
		t.Fatal(err)
	}
	cands, cov := build(t, p, Inputs{Samples: apiHerd()})
	if len(cands) != 0 || !slices.Contains(cov.Missing, "connections") {
		t.Fatalf("no section: %+v %+v", cands, cov)
	}
}

// Invalid input: a nil pack, no database and a window under a day are
// errors naming what is wrong.
func TestBuild_InvalidInputIsAnError(t *testing.T) {
	p := plainPack(t)
	for name, tc := range map[string]struct {
		pack *workpack.Pack
		env  candidate.Env
		want string
	}{"nil pack": {nil, env(), "no evidence pack"},
		"no database": {p, candidate.Env{WindowDays: 90}, "no database"},
		"zero window": {p, candidate.Env{Database: "db1"}, "window of 0 days"},
	} {
		_, _, err := Build(tc.pack, Inputs{Samples: apiHerd()}, tc.env)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}

// Generate reads the source over the pack's window and builds from it.
func TestGenerate_ReadsTheSourceOverThePackWindow(t *testing.T) {
	p := plainPack(t)
	src := &fakeSource{in: Inputs{Samples: apiHerd()}}
	cands, _, err := New(src).Generate(context.Background(), p, env())
	if err != nil || Herds(cands) != 1 {
		t.Fatalf("Generate: %d herds, err %v", Herds(cands), err)
	}
	if src.calls != 1 || !src.from.Equal(p.Window.From) || !src.to.Equal(p.Window.To) {
		t.Fatalf("source asked %d times for %v..%v, want the pack window %v..%v",
			src.calls, src.from, src.to, p.Window.From, p.Window.To)
	}
}

// Error propagation: a source error is the generator's, naming the
// database; a nil source reads nothing and says connections are missing.
func TestGenerate_SourceErrorAndNilSource(t *testing.T) {
	boom := errors.New("connection refused")
	_, _, err := New(&fakeSource{err: boom}).Generate(context.Background(), plainPack(t),
		env())
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "db1") {
		t.Fatalf("source error: %v", err)
	}
	cands, cov, err := New(nil).Generate(context.Background(), plainPack(t), env())
	if err != nil || len(cands) != 0 || !slices.Contains(cov.Missing, "connections") {
		t.Fatalf("nil source: %+v %+v %v", cands, cov, err)
	}
	if _, _, err := New(nil).Generate(context.Background(), nil, env()); err == nil {
		t.Fatal("nil pack: no error")
	}
}

// A canceled context stops Generate before it reads.
func TestGenerate_CanceledContextIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &fakeSource{in: Inputs{Samples: apiHerd()}}
	if _, _, err := New(src).Generate(ctx, plainPack(t), env()); !errors.Is(err,
		context.Canceled) || src.calls != 0 {
		t.Fatalf("canceled: err %v, %d reads", err, src.calls)
	}
}

// Every candidate the generator emits validates against its pack, and the
// same inputs give the same ids (stable across reviews).
func TestBuild_CandidatesValidateAndAreStable(t *testing.T) {
	in := limitInputs(map[string]int{"batch": 60, "api": 30, "web": 5})
	in.Samples, in.Mix = apiHerd(), webMix()
	in.IdleInTx = []IdleInTx{{Role: "api", AgeSeconds: 900, At: t0}}
	p := packWith(t, measures(100, 95))
	a, _ := build(t, p, in)
	b, _ := build(t, p, in)
	kinds := map[string]bool{}
	for i := range a {
		kinds[a[i].Kind] = true
		if err := a[i].Validate(p); err != nil {
			t.Errorf("%s: %v", a[i].Kind, err)
		}
		if a[i].ID != b[i].ID || a[i].Dimension != Dimension {
			t.Errorf("%s: ids %s / %s, dimension %q", a[i].Kind, a[i].ID, b[i].ID,
				a[i].Dimension)
		}
	}
	for _, k := range []string{KindRecycleHerd, KindPoolerSizing, KindIdleInTxTimeout,
		KindConnectionLimit} {
		if !kinds[k] {
			t.Errorf("no %s candidate in %+v", k, a)
		}
	}
}

// No concurrent access tests: Build is a pure function of its arguments and
// Generator keeps no state between calls (TestGenerate_* share nothing).
// No state transition tests: the generator proposes; plans and the
// executor own every transition of a step.
