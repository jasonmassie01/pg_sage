package capacity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// No concurrent access tests: Build is a pure function of its arguments
// and Generate keeps no state between calls (one read, then Build).

func TestBuild_RefusesInvalidInput(t *testing.T) {
	pack := steadyPack(t)
	cases := map[string]struct {
		env  candidate.Env
		want string
	}{
		"nil pack":    {env: testEnv(), want: "no evidence pack"},
		"no database": {env: candidate.Env{WindowDays: 28}, want: "no database named"},
		"zero window": {env: candidate.Env{Database: "app"}, want: "window of 0 days"},
	}
	for name, tc := range cases {
		p := pack
		if name == "nil pack" {
			p = nil
		}
		cands, _, err := Build(p, Inputs{}, tc.env)
		if err == nil || !strings.Contains(err.Error(), tc.want) || cands != nil {
			t.Errorf("%s: %v, %v; want an error saying %q", name, cands, err, tc.want)
		}
	}
}

// failingCalls is a CallSource that fails.
type failingCalls struct{ err error }

func (f failingCalls) Read(context.Context, time.Time, time.Time) ([]DayCalls, error) {
	return nil, f.err
}

// fixedCalls returns its days and records the window it was asked for.
type fixedCalls struct {
	days     []DayCalls
	from, to *time.Time
}

func (f fixedCalls) Read(_ context.Context, from, to time.Time) ([]DayCalls, error) {
	*f.from, *f.to = from, to
	return f.days, nil
}

func TestGenerate_CallSourceErrorIsAnErrorNamingTheRead(t *testing.T) {
	cause := errors.New("connection refused")
	g := New(Config{Calls: failingCalls{err: cause}})
	_, _, err := g.Generate(context.Background(), steadyPack(t), testEnv())
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "read the workload calls") ||
		!strings.Contains(err.Error(), "app") {
		t.Fatalf("err %v, want the read named and the cause wrapped", err)
	}
}

func TestGenerate_CanceledContextAndNilPack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := New(Config{}).Generate(ctx, steadyPack(t), testEnv()); !errors.Is(err,
		context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if _, _, err := New(Config{}).Generate(context.Background(), nil, testEnv()); err == nil {
		t.Fatal("a nil pack was reviewed")
	}
}

func TestGenerate_ReadsThePackWindowAndJudgesIdle(t *testing.T) {
	var from, to time.Time
	g := New(Config{Calls: fixedCalls{days: quiet(20, 0), from: &from, to: &to}})
	cands, _, err := g.Generate(context.Background(), steadyPack(t), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if !from.Equal(window.From) || !to.Equal(window.To) {
		t.Fatalf("read [%s, %s), want the pack window", from, to)
	}
	if len(byKind(cands, KindIdleDatabase)) != 1 {
		t.Fatalf("no idle candidate from 20 quiet days: %+v", cands)
	}
}

func TestGenerate_RatesComeFromTheRateSource(t *testing.T) {
	src := cost.NewConfigRates(configRates(t))
	g := New(Config{Calls: fixedCalls{days: quiet(20, 0), from: new(time.Time),
		to: new(time.Time)}, Rates: src})
	cands, _, err := g.Generate(context.Background(), steadyPack(t), testEnv())
	if err != nil {
		t.Fatal(err)
	}
	idle := byKind(cands, KindIdleDatabase)
	if len(idle) != 1 || idle[0].Prediction.Quantities[QuantityMonthlyCost] <= 0 {
		t.Fatalf("idle with the database's cost.rates entry: %+v", idle)
	}
}

func TestBuild_EveryOutputIsAdviceAndNeedsApproval(t *testing.T) {
	delta := 100.0
	rs := &Rightsizing{Resource: "db-1", Verdict: downsizeVerdict(&delta),
		AllocatedStorageBytes: 200 * gib}
	pack := growthPack(t, series(dbID, "", linear(28, 100*gib, gib, mib)),
		series(tableID, "public.orders", linear(28, gib, gib, 0)))
	cands, _, err := Build(pack, Inputs{Rightsizing: rs, Days: quiet(20, 0)}, testEnv())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 5 {
		t.Fatalf("%d candidates, want growth, table, storage, right-size and idle: %+v",
			len(cands), cands)
	}
	for _, c := range cands {
		if c.ActionType != candidate.ActionAdvice || c.Approval != candidate.ApprovalRequired ||
			c.ForwardSQL != "" || c.RollbackSQL != "" || c.Operator != nil {
			t.Errorf("%s %s is not approval-gated advice: %+v", c.Kind, c.Target, c)
		}
		if c.ID != candidate.NewID(Name, c.Kind, c.Target, "") {
			t.Errorf("%s id is not stable", c.Kind)
		}
	}
}

// configRates is a cost.rates entry for the database "app".
func configRates(t *testing.T) config.CostConfig {
	t.Helper()
	compute := 0.5
	return config.CostConfig{Rates: []config.CostRate{{Database: "app", Currency: "USD",
		AsOf: "2026-10-01", ComputePerHour: &compute, StorageGBMonth: 0.10}}}
}
