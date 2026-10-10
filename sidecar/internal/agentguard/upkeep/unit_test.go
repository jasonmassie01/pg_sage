package upkeep

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
)

// fakeApplier stands in for a database's executor in the unit tests; the
// role contracts it would run are faked by fakeRoles.
type fakeApplier struct{ name string }

func (fakeApplier) Apply(context.Context, executor.ActionIntent) (int64, error) {
	return 0, errors.New("fakeApplier.Apply must not be called")
}

func (fakeApplier) StandingPolicyGate() policy.Gate { return nil }

func noTargets(context.Context) ([]agentguard.KillTarget, error) { return nil, nil }

func TestNew_Validates(t *testing.T) {
	pool := &pgxpool.Pool{}
	roles := &fakeRoles{}
	good := Config{RetireGrace: 7 * 24 * time.Hour, Rotation: 7 * 24 * time.Hour}
	cases := []struct {
		name    string
		control *pgxpool.Pool
		roles   Roles
		targets Targets
		cfg     Config
	}{
		{"nil control", nil, roles, noTargets, good},
		{"nil roles", pool, nil, noTargets, good},
		{"nil targets", pool, roles, nil, good},
		{"negative grace", pool, roles, noTargets, Config{RetireGrace: -time.Second,
			Rotation: time.Hour}},
		{"zero rotation", pool, roles, noTargets, Config{RetireGrace: 0}},
		{"negative batch", pool, roles, noTargets, Config{Rotation: time.Hour, Batch: -1}},
	}
	for _, c := range cases {
		r, err := New(c.control, c.roles, c.targets, c.cfg)
		if !errors.Is(err, ErrInvalid) || r != nil {
			t.Errorf("%s: New = %v, %v; want ErrInvalid and no runner", c.name, r, err)
		}
	}
}

func TestNew_DefaultsAndZeroGrace(t *testing.T) {
	r, err := New(&pgxpool.Pool{}, &fakeRoles{}, noTargets, Config{Rotation: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.cfg.Batch != DefaultBatch || r.cfg.RetireGrace != 0 || r.cfg.Rotation != time.Hour {
		t.Fatalf("config = %+v, want batch %d, grace 0 (drop at the next pass), "+
			"rotation 1h", r.cfg, DefaultBatch)
	}
}

func TestConfigFrom(t *testing.T) {
	c := ConfigFrom(7, 3)
	if c.RetireGrace != 7*24*time.Hour || c.Rotation != 3*24*time.Hour ||
		c.Batch != DefaultBatch {
		t.Fatalf("ConfigFrom(7, 3) = %+v", c)
	}
	if z := ConfigFrom(0, 1); z.RetireGrace != 0 || z.Rotation != 24*time.Hour {
		t.Fatalf("ConfigFrom(0, 1) = %+v", z)
	}
}

func TestClusterFor_GroupsAndPicksTheExecutor(t *testing.T) {
	p1, p2, p3 := &pgxpool.Pool{}, &pgxpool.Pool{}, &pgxpool.Pool{}
	targets := []agentguard.KillTarget{
		{Name: "a", Pool: p1, ClusterKey: "k1"},
		{Name: "b", Pool: p2, ClusterKey: "k1", Executor: fakeApplier{"b"}},
		{Name: "c", Pool: p3, ClusterKey: "k2", Executor: fakeApplier{"c"}},
		{Name: "d", Pool: nil, ClusterKey: "k1", Executor: fakeApplier{"d"}},
	}
	c, ex, ok := clusterFor(targets, "k1")
	if !ok {
		t.Fatal("cluster k1 not found")
	}
	if c.Key != "k1" || c.Admin != p2 || ex.(fakeApplier).name != "b" {
		t.Fatalf("cluster = %+v, executor %v; want admin b's pool and b's executor", c, ex)
	}
	if len(c.Databases) != 2 || c.Databases[0].Name != "a" || c.Databases[1].Name != "b" {
		t.Fatalf("databases = %+v, want a and b (d has no pool)", c.Databases)
	}
	if _, _, ok := clusterFor(targets, "k3"); ok {
		t.Fatal("an unknown cluster key must not resolve")
	}
	if _, _, ok := clusterFor(targets, ""); ok {
		t.Fatal("an empty cluster key must not resolve")
	}
	noExec := []agentguard.KillTarget{{Name: "a", Pool: p1, ClusterKey: "k1"}}
	if _, _, ok := clusterFor(noExec, "k1"); ok {
		t.Fatal("a cluster without an executor cannot run a role contract")
	}
	if _, _, ok := clusterFor(nil, "k1"); ok {
		t.Fatal("no targets resolves nothing")
	}
}

func TestClassify(t *testing.T) {
	base := Outcome{PrincipalID: "agp_aaaaaaaaaaaaaaaaaaaa", ClusterKey: "k"}
	withheld := &executor.WithheldError{Decision: executor.ActionPolicyDecision{
		BlockedReason: "trust_level_observation"}}
	residue := fmt.Errorf("%w: %w", agentguard.ErrPostCheck, &agentguard.DeniedError{
		Reason: agentguard.ReasonRevokeIncomplete, Detail: "privileges from another grantor",
		Fix: "REVOKE SELECT ON t FROM r"})
	cases := []struct {
		err    error
		bucket string
		reason string
		fix    string
	}{
		{nil, bucketDone, "", ""},
		{fmt.Errorf("apply: %w", withheld), bucketWithheld, "trust_level_observation", ""},
		{residue, bucketIncomplete, string(agentguard.ReasonRevokeIncomplete),
			"REVOKE SELECT ON t FROM r"},
		{fmt.Errorf("%w: server version 150000", agentguard.ErrRoleManagementUnsupported),
			bucketSkipped, ReasonUnsupported, ""},
		{agentguard.ErrEncryptionKeyRequired, bucketSkipped, ReasonNoKey, ""},
		{fmt.Errorf("%w: principal x", agentguard.ErrRetired), bucketSkipped, ReasonRetired, ""},
		{&agentguard.DeniedError{Reason: agentguard.ReasonFrozen, Detail: "frozen"},
			bucketSkipped, string(agentguard.ReasonFrozen), ""},
		{errors.New("connection refused"), bucketFailed, ReasonError, ""},
	}
	for _, c := range cases {
		bucket, o := classify(base, c.err)
		if bucket != c.bucket || o.Reason != c.reason || o.Fix != c.fix {
			t.Errorf("classify(%v) = %s %+v; want %s reason %q fix %q", c.err, bucket, o,
				c.bucket, c.reason, c.fix)
		}
		if o.PrincipalID != base.PrincipalID || o.ClusterKey != base.ClusterKey {
			t.Errorf("classify lost the principal or cluster: %+v", o)
		}
		if c.err != nil && o.Detail == "" {
			t.Errorf("classify(%v): an error outcome needs a detail", c.err)
		}
	}
}

func TestReport_AddAndCount(t *testing.T) {
	var rep Report
	o := Outcome{PrincipalID: "agp_aaaaaaaaaaaaaaaaaaaa"}
	for _, b := range []string{bucketDone, bucketSkipped, bucketIncomplete, bucketWithheld,
		bucketFailed, bucketFailed} {
		rep.add(b, o)
	}
	if len(rep.Done) != 1 || len(rep.Skipped) != 1 || len(rep.Incomplete) != 1 ||
		len(rep.Withheld) != 1 || len(rep.Failed) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Total() != 6 {
		t.Fatalf("Total = %d, want 6", rep.Total())
	}
}

func TestFence_EmptyHolderIsUnfenced(t *testing.T) {
	// No election (a single sidecar): every write is allowed and the lease
	// table is not read, so a nil querier is never touched.
	if err := (Fence{}).check(context.Background(), nil); err != nil {
		t.Fatalf("empty fence: %v", err)
	}
}

// No concurrent-access unit test here: clusterFor and classify are pure
// functions; the runner's concurrent passes are covered in db_test.go.
