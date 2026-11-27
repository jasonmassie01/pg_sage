package upgradecheck

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// drill-02b: the Aurora and RDS rehearsers upgrade the clone only, and
// only a resource a live rehearsal receipt names.

// No concurrent access tests: an AWSUpgrader holds no mutable state; the
// registry serializes rehearsals of one database.

type fakeEngineAPI struct {
	engine   clone.EngineState
	states   []clone.EngineState // returned in turn after the modify
	targets  []string
	modErr   error
	descErr  error
	modified []string // "id=version"
}

func (f *fakeEngineAPI) Engine(_ context.Context, id string) (clone.EngineState, error) {
	if f.descErr != nil {
		return clone.EngineState{}, f.descErr
	}
	if len(f.modified) == 0 || len(f.states) == 0 {
		return f.engine, nil
	}
	s := f.states[0]
	if len(f.states) > 1 {
		f.states = f.states[1:]
	}
	return s, nil
}

func (f *fakeEngineAPI) UpgradeTargets(_ context.Context, engine, version string) ([]string,
	error) {
	if engine != f.engine.Engine || version != f.engine.Version {
		return nil, errors.New("asked for targets of another engine version")
	}
	return f.targets, nil
}

func (f *fakeEngineAPI) ModifyEngineVersion(_ context.Context, id, version string) error {
	if f.modErr != nil {
		return f.modErr
	}
	f.modified = append(f.modified, id+"="+version)
	return nil
}

func liveOnly(ids ...string) func(context.Context, string) (bool, error) {
	return func(_ context.Context, id string) (bool, error) {
		for _, live := range ids {
			if id == live {
				return true, nil
			}
		}
		return false, nil
	}
}

func auroraUpgrader(api *fakeEngineAPI) AWSUpgrader {
	return AWSUpgrader{Kind: "aurora", API: api, SourceID: "prod-cluster",
		Live: liveOnly("sage-rehearsal-0a1b2c3d"), PollEvery: time.Millisecond,
		MaxWait: time.Second}
}

func aurora14() *fakeEngineAPI {
	return &fakeEngineAPI{
		engine: clone.EngineState{Engine: "aurora-postgresql", Version: "14.9",
			Status: "available"},
		targets: []string{"15.4", "17.2", "17.10", "16.1", "17.4"},
		states: []clone.EngineState{{Engine: "aurora-postgresql", Version: "14.9",
			Status: "upgrading"}, {Engine: "aurora-postgresql", Version: "17.10",
			Status: "available"}}}
}

var rehearsalClone = clone.Clone{ID: "sage-rehearsal-0a1b2c3d",
	DSN: "postgres://sage@clone.cluster-x.rds.amazonaws.com:5432/app"}

func TestAWSUpgrader_UpgradesTheCloneToTheNewestTargetOfTheMajor(t *testing.T) {
	api := aurora14()
	got, err := auroraUpgrader(api).Upgrade(context.Background(), rehearsalClone, 17)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if got != rehearsalClone {
		t.Fatalf("Upgrade = %+v, want the same clone (in place)", got)
	}
	if len(api.modified) != 1 || api.modified[0] != "sage-rehearsal-0a1b2c3d=17.10" {
		t.Fatalf("modified %v, want the clone to 17.10 (numeric minor order)", api.modified)
	}
}

func TestAWSUpgrader_RefusesTheSourceAndUnreceiptedResources(t *testing.T) {
	cases := map[string]struct {
		mutate func(*AWSUpgrader, *clone.Clone)
		want   string
	}{
		"source id": {func(u *AWSUpgrader, c *clone.Clone) { c.ID = "prod-cluster" },
			"source"},
		"source id, other case": {func(u *AWSUpgrader, c *clone.Clone) {
			c.ID = "PROD-Cluster"
			u.Live = liveOnly("PROD-Cluster")
		}, "source"},
		"no source configured": {func(u *AWSUpgrader, _ *clone.Clone) { u.SourceID = "" },
			"source"},
		"no receipt check": {func(u *AWSUpgrader, _ *clone.Clone) { u.Live = nil }, "receipt"},
		"no live receipt": {func(u *AWSUpgrader, _ *clone.Clone) { u.Live = liveOnly() },
			"receipt"},
		"empty clone id": {func(_ *AWSUpgrader, c *clone.Clone) { c.ID = "" }, "id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := aurora14()
			u, cl := auroraUpgrader(api), rehearsalClone
			tc.mutate(&u, &cl)
			_, err := u.Upgrade(context.Background(), cl, 17)
			if !errors.Is(err, ErrUpgradeRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Upgrade = %v, want ErrUpgradeRefused naming %q", err, tc.want)
			}
			if len(api.modified) != 0 {
				t.Fatalf("a refused upgrade modified %v", api.modified)
			}
		})
	}
}

func TestAWSUpgrader_ErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	liveErr := errors.New("control database down")
	u := auroraUpgrader(aurora14())
	u.Live = func(context.Context, string) (bool, error) { return false, liveErr }
	if _, err := u.Upgrade(ctx, rehearsalClone, 17); !errors.Is(err, liveErr) {
		t.Errorf("receipt read error = %v, want it wrapped", err)
	}
	api := aurora14()
	api.modErr = errors.New("InvalidDBClusterStateFault: cluster is not available")
	if _, err := auroraUpgrader(api).Upgrade(ctx, rehearsalClone, 17); err == nil ||
		!strings.Contains(err.Error(), "InvalidDBClusterStateFault") {
		t.Errorf("modify error = %v, want the provider message", err)
	}
	api = aurora14()
	api.descErr = errors.New("AccessDenied")
	if _, err := auroraUpgrader(api).Upgrade(ctx, rehearsalClone, 17); err == nil ||
		!strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("describe error = %v, want it wrapped", err)
	}
}

func TestAWSUpgrader_Boundaries(t *testing.T) {
	ctx := context.Background()
	api := aurora14()
	api.targets = []string{"15.4", "16.1"}
	if _, err := auroraUpgrader(api).Upgrade(ctx, rehearsalClone, 17); !errors.Is(err,
		ErrUpgradeUnsupported) || len(api.modified) != 0 {
		t.Errorf("no target of the major = %v, want ErrUpgradeUnsupported", err)
	}
	for _, major := range []int{0, -3, 14, 13} {
		api = aurora14()
		if _, err := auroraUpgrader(api).Upgrade(ctx, rehearsalClone, major); err == nil ||
			len(api.modified) != 0 {
			t.Errorf("target major %d from 14 accepted", major)
		}
	}
	api = aurora14()
	api.states = []clone.EngineState{{Version: "14.9", Status: "upgrading"}}
	if _, err := auroraUpgrader(api).Upgrade(ctx, rehearsalClone, 17); err == nil ||
		!strings.Contains(err.Error(), "did not finish") {
		t.Errorf("upgrade never finishing = %v, want a timeout", err)
	}
	api = aurora14()
	api.states = []clone.EngineState{{Version: "14.9", Status: "incompatible-parameters"}}
	if _, err := auroraUpgrader(api).Upgrade(ctx, rehearsalClone, 17); err == nil ||
		!strings.Contains(err.Error(), "incompatible-parameters") {
		t.Errorf("failed upgrade status = %v, want it named", err)
	}
}

// State transitions: available at the old major after the modify is
// still upgrading; only available at the target major is done.
func TestAWSUpgrader_WaitsForTheTargetMajor(t *testing.T) {
	api := aurora14()
	api.states = []clone.EngineState{{Version: "14.9", Status: "available"},
		{Version: "14.9", Status: "upgrading"}, {Version: "17.10", Status: "available"}}
	if _, err := auroraUpgrader(api).Upgrade(context.Background(), rehearsalClone,
		17); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if len(api.states) != 1 || api.states[0].Version != "17.10" {
		t.Fatalf("stopped polling early: %v left", api.states)
	}
}

func TestNeonUpgrader_CannotChangeAMajor(t *testing.T) {
	_, err := NeonUpgrader{}.Upgrade(context.Background(), rehearsalClone, 17)
	if !errors.Is(err, ErrUpgradeUnsupported) || !strings.Contains(err.Error(), "Neon") {
		t.Fatalf("Neon Upgrade = %v, want ErrUpgradeUnsupported", err)
	}
}
