package decide

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

func decideWith(t *testing.T, cfg Config, req Request) Verdict {
	t.Helper()
	return New(cfg).Decide(context.Background(), req)
}

func wantDenied(t *testing.T, v Verdict, reason agentguard.Reason, step string) {
	t.Helper()
	if v.Allowed || v.Park || v.Reason != reason || v.Step != step {
		t.Fatalf("verdict = %+v, want denied %s at %s", v, reason, step)
	}
	if strings.TrimSpace(v.Detail) == "" {
		t.Fatalf("verdict %+v carries no detail: a denial says why once", v)
	}
}

func wantAllowed(t *testing.T, v Verdict, level int) {
	t.Helper()
	if !v.Allowed || v.Park || v.MaxLevel != level || v.Reason != "" {
		t.Fatalf("verdict = %+v, want allowed at L%d", v, level)
	}
}

func TestDecideHappyPaths(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	v := decideWith(t, cfg, agentRead())
	// G1: a brokered read under an approved grant runs at L3.
	wantAllowed(t, v, 3)
	if v.Env != envbind.EnvStage || v.Capability != CapRead || v.Principal.ID != testID {
		t.Fatalf("verdict = %+v, want env stage, class read and the principal", v)
	}
	// G1: every other class is at most L2 (every grant is operator-approved).
	wantAllowed(t, decideWith(t, cfg, proposeMigration()), 2)
	write := agentRead()
	write.Capability = CapWriteInsert
	wantAllowed(t, decideWith(t, cfg, write), 2)
}

func TestDecideRequiresPrincipalSource(t *testing.T) {
	v := decideWith(t, Config{}, proposeMigration())
	wantDenied(t, v, ReasonUnavailable, "D1")
}

func TestDecideD1StoreErrorsFailClosed(t *testing.T) {
	cfg, principals := fullConfig(activePrincipal())
	principals.err = errBoom
	v := decideWith(t, cfg, proposeMigration())
	wantDenied(t, v, ReasonUnavailable, "D1")
	if !strings.Contains(v.Detail, "connection refused") {
		t.Fatalf("detail = %q, want the cause", v.Detail)
	}
}

func TestDecideD1UnknownOrRetiredPrincipal(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	req := proposeMigration()
	req.PrincipalID = "agp_bbbbbbbbbbbbbbbbbbbb"
	wantDenied(t, decideWith(t, cfg, req), agentguard.ReasonRetired, "D1")
	p := activePrincipal()
	p.Status = agentguard.StatusRetired
	cfg, _ = fullConfig(p)
	wantDenied(t, decideWith(t, cfg, proposeMigration()), agentguard.ReasonRetired, "D1")
}

func TestDecideD1FrozenPrincipal(t *testing.T) {
	p := activePrincipal()
	p.Status, p.FrozenReason = agentguard.StatusFrozen, "kill: canary read"
	cfg, _ := fullConfig(p)
	wantDenied(t, decideWith(t, cfg, proposeMigration()), agentguard.ReasonFrozen, "D1")
	wantDenied(t, decideWith(t, cfg, agentRead()), agentguard.ReasonFrozen, "D1")
}

func TestDecideD1FreezeFlagOnDatabaseOrFleet(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.Freezes = fakeFreezes{frozen: true, reason: "fleet kill switch"}
	v := decideWith(t, cfg, proposeMigration())
	wantDenied(t, v, agentguard.ReasonFrozen, "D1")
	if !strings.Contains(v.Detail, "fleet kill switch") {
		t.Fatalf("detail = %q, want the flag's reason", v.Detail)
	}
	cfg.Freezes = fakeFreezes{err: errBoom}
	wantDenied(t, decideWith(t, cfg, proposeMigration()), ReasonUnavailable, "D1")
}

func TestDecideD2Unsponsored(t *testing.T) {
	p := activePrincipal()
	p.SponsorUserID = nil
	cfg, _ := fullConfig(p)
	wantDenied(t, decideWith(t, cfg, agentRead()), agentguard.ReasonUnsponsored, "D2")
	// Existing propose tools queue for a person (§6.4, G1-11).
	wantAllowed(t, decideWith(t, cfg, proposeMigration()), 2)
	p.SponsorUserID, p.SponsorActive = sponsor(), false // departed sponsor
	cfg, _ = fullConfig(p)
	wantDenied(t, decideWith(t, cfg, agentRead()), agentguard.ReasonUnsponsored, "D2")
}

func TestDecideUnboundStdioAgent(t *testing.T) {
	cfg, principals := fullConfig(activePrincipal())
	cfg.Environments = fakeEnvs{env: envbind.EnvDev}
	req := proposeMigration()
	req.PrincipalID = ""
	wantAllowed(t, decideWith(t, cfg, req), 2)
	if principals.calls != 0 {
		t.Fatal("an unbound agent has no principal row to load")
	}
	read := agentRead()
	read.PrincipalID = ""
	wantDenied(t, decideWith(t, cfg, read), agentguard.ReasonUnsponsored, "D2")
}

func TestDecideD3EnvironmentCeiling(t *testing.T) {
	cases := []struct {
		name    string
		env     envbind.Env
		ceiling agentguard.Env
		allowed bool
	}{
		{"prod under prod ceiling, profile stage", envbind.EnvProd, agentguard.EnvProd, false},
		{"stage under stage profile", envbind.EnvStage, agentguard.EnvProd, true},
		{"dev under dev", envbind.EnvDev, agentguard.EnvDev, true},
		{"stage over dev", envbind.EnvStage, agentguard.EnvDev, false},
		{"branch under branch", envbind.EnvBranch, agentguard.EnvBranch, true},
		{"invalid ceiling counts as branch", envbind.EnvDev, agentguard.Env("x"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := activePrincipal()
			p.EnvCeiling = tc.ceiling
			cfg, _ := fullConfig(p)
			cfg.Environments = fakeEnvs{env: tc.env}
			v := decideWith(t, cfg, proposeMigration())
			if tc.allowed {
				wantAllowed(t, v, 2)
				return
			}
			wantDenied(t, v, ReasonEnvCeiling, "D3")
			if v.Env != tc.env {
				t.Fatalf("env = %q, want %q", v.Env, tc.env)
			}
		})
	}
}

func TestDecideD3UnknownEnvironmentIsProd(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.Environments = nil
	v := decideWith(t, cfg, proposeMigration())
	wantDenied(t, v, ReasonEnvCeiling, "D3")
	if v.Env != envbind.EnvProd {
		t.Fatalf("env = %q, want prod without an environment source", v.Env)
	}
	cfg.Environments = fakeEnvs{err: errBoom}
	if v = decideWith(t, cfg, proposeMigration()); v.Env != envbind.EnvProd {
		t.Fatalf("env = %q, want prod when the binding cannot be read", v.Env)
	}
}

func TestDecideD3NoDatabaseSkipsEnvironment(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.Environments = nil
	req := Request{PrincipalID: testID, Tool: "propose_policy_change",
		Kind: agentguard.ToolPropose, Capability: CapPolicyProposal}
	v := decideWith(t, cfg, req)
	wantAllowed(t, v, 2)
	if v.Env != "" {
		t.Fatalf("env = %q, want none without a database", v.Env)
	}
}

func TestDecideD4Capability(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	req := agentRead()
	req.Capability = CapDDLDestructive // not in coding-agent
	wantDenied(t, decideWith(t, cfg, req), ReasonCapability, "D4")
	req.Capability = "teleport"
	wantDenied(t, decideWith(t, cfg, req), ReasonCapability, "D4")
	req.Capability = CapRead
	cfg.Profiles = nil
	wantDenied(t, decideWith(t, cfg, req), ReasonCapability, "D4")
	cfg.Profiles = fakeProfiles{}
	wantDenied(t, decideWith(t, cfg, req), ReasonCapability, "D4")
	// Existing propose tools are governed by the §6.2.6 table, not the
	// profile: a legacy (read-only) principal's migration still queues.
	p := activePrincipal()
	p.Profile = "legacy"
	cfg, _ = fullConfig(p)
	cfg.Profiles = fakeProfiles{"legacy": {Classes: []Capability{CapRead},
		EnvCeiling: envbind.EnvProd}}
	wantAllowed(t, decideWith(t, cfg, proposeMigration()), 2)
}

func TestDecideD5Classification(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.Objects = fakeObjects{err: &agentguard.DeniedError{Reason: "secret_column",
		Detail: "app.users.password_hash is secret", Fix: "remove the column"}}
	v := decideWith(t, cfg, agentRead())
	wantDenied(t, v, ReasonClassification, "D5")
	if v.Fix != "remove the column" || !strings.Contains(v.Detail, "password_hash") {
		t.Fatalf("verdict = %+v, want the checker's detail and fix", v)
	}
	cfg.Objects = nil
	wantDenied(t, decideWith(t, cfg, agentRead()), ReasonClassification, "D5")
	cfg.Objects = fakeObjects{err: errBoom}
	wantDenied(t, decideWith(t, cfg, agentRead()), ReasonUnavailable, "D5")
	req := agentRead()
	req.Objects = nil // no objects: nothing to check
	cfg.Objects = nil
	wantAllowed(t, decideWith(t, cfg, req), 3)
}

func TestDecideD6ChangeFreeze(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.ChangeFreeze = func(context.Context, string) (bool, error) { return true, nil }
	wantDenied(t, decideWith(t, cfg, proposeMigration()), ReasonChangeFreeze, "D6")
	wantAllowed(t, decideWith(t, cfg, agentRead()), 3) // reads are not changes
	cfg.ChangeFreeze = func(context.Context, string) (bool, error) { return false, errBoom }
	wantDenied(t, decideWith(t, cfg, proposeMigration()), ReasonUnavailable, "D6")
	cfg.ChangeFreeze = nil // no source: the flag is off
	wantAllowed(t, decideWith(t, cfg, proposeMigration()), 2)
}

func prodConfig() Config {
	cfg, _ := fullConfig(activePrincipal())
	cfg.Environments = fakeEnvs{env: envbind.EnvProd}
	cfg.Profiles = fakeProfiles{"coding-agent": {Classes: codingProfiles()["coding-agent"].
		Classes, EnvCeiling: envbind.EnvProd}}
	return cfg
}

func TestDecideD7RecoveryInProd(t *testing.T) {
	cfg := prodConfig()
	wantAllowed(t, decideWith(t, cfg, proposeMigration()), 2)
	cfg.Recovery = fakeRecovery{pitr: false}
	wantDenied(t, decideWith(t, cfg, proposeMigration()), ReasonNoPITR, "D7")
	cfg.Recovery = nil
	wantDenied(t, decideWith(t, cfg, proposeMigration()), ReasonNoPITR, "D7")
	cfg.Recovery = fakeRecovery{err: errBoom}
	wantDenied(t, decideWith(t, cfg, proposeMigration()), ReasonUnavailable, "D7")
	// Reads and maintenance need no PITR; stage writes neither.
	wantAllowed(t, decideWith(t, cfg, agentRead()), 3)
	maint := proposeMigration()
	maint.Capability = CapMaint
	wantAllowed(t, decideWith(t, cfg, maint), 2)
	stage, _ := fullConfig(activePrincipal())
	stage.Recovery = nil
	wantAllowed(t, decideWith(t, stage, proposeMigration()), 2)
}

func TestDecideD7StaleDrillCapsAtL2(t *testing.T) {
	cfg := prodConfig()
	read := agentRead()
	read.Capability = CapWriteInsert
	cases := map[string]struct {
		drill time.Time
		level int
	}{
		"never":         {time.Time{}, 2},
		"15 days":       {testNow.Add(-15 * 24 * time.Hour), 2},
		"exactly 14":    {testNow.Add(-14 * 24 * time.Hour), 2},
		"13 days 23h59": {testNow.Add(-14*24*time.Hour + time.Minute), 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg.Recovery = fakeRecovery{pitr: true, drill: tc.drill}
			v := decideWith(t, cfg, read)
			wantAllowed(t, v, tc.level)
		})
	}
	if got := New(cfg).drillCap(testNow.Add(-14*24*time.Hour+time.Minute), 3); got != 3 {
		t.Fatalf("drillCap(fresh) = %d, want 3", got)
	}
	if got := New(cfg).drillCap(testNow.Add(-14*24*time.Hour), 3); got != 3 {
		t.Fatalf("drillCap(exactly 14 days) = %d, want 3: only older drills cap", got)
	}
	if got := New(cfg).drillCap(time.Time{}, 3); got != 2 {
		t.Fatalf("drillCap(never) = %d, want 2", got)
	}
	if got := New(cfg).drillCap(testNow.Add(-14*24*time.Hour-time.Minute), 3); got != 2 {
		t.Fatalf("drillCap(stale) = %d, want 2", got)
	}
}

func TestDecideD8TaintCapsAtL2(t *testing.T) {
	p := activePrincipal()
	p.Tainted = true
	cfg, _ := fullConfig(p)
	v := decideWith(t, cfg, agentRead())
	wantAllowed(t, v, 2)
	wantAllowed(t, decideWith(t, cfg, proposeMigration()), 2)
}

func TestDecideD9PendingApprovalBudget(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	for _, tc := range []struct {
		pending int
		allowed bool
	}{{0, true}, {9, true}, {10, false}, {11, false}} {
		n := tc.pending
		cfg.Pending = func(context.Context, string) (int, error) { return n, nil }
		v := decideWith(t, cfg, proposeMigration())
		if tc.allowed {
			wantAllowed(t, v, 2)
			continue
		}
		wantDenied(t, v, ReasonBudget, "D9")
	}
	cfg.Pending = func(context.Context, string) (int, error) { return 0, errBoom }
	wantDenied(t, decideWith(t, cfg, proposeMigration()), ReasonUnavailable, "D9")
}

func TestDecideD9RatePerHourParks(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	now := testNow
	cfg.Now = func() time.Time { return now }
	cfg.MaxRequestsPerHour = 3
	d := New(cfg)
	for i := range 3 {
		v := d.Decide(context.Background(), proposeMigration())
		if !v.Allowed {
			t.Fatalf("request %d = %+v, want allowed under the limit", i+1, v)
		}
		now = now.Add(time.Minute)
	}
	v := d.Decide(context.Background(), proposeMigration())
	if v.Allowed || !v.Park || v.Reason != ReasonRate || v.Step != "D9" {
		t.Fatalf("4th request = %+v, want parked agent_rate", v)
	}
	if want := 57 * time.Minute; v.RetryAfter != want {
		t.Fatalf("retry_after = %v, want %v (the oldest request leaves the hour)",
			v.RetryAfter, want)
	}
	// Reads are not approval requests and are not counted.
	if v := d.Decide(context.Background(), agentRead()); !v.Allowed {
		t.Fatalf("read = %+v, want allowed", v)
	}
	now = testNow.Add(time.Hour)
	if v := d.Decide(context.Background(), proposeMigration()); !v.Allowed {
		t.Fatalf("after the window = %+v, want allowed", v)
	}
}

func TestDecideD9RateIsPerPrincipal(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.MaxRequestsPerHour = 1
	d := New(cfg)
	if v := d.Decide(context.Background(), proposeMigration()); !v.Allowed {
		t.Fatalf("first = %+v", v)
	}
	other := proposeMigration()
	other.PrincipalID = ""
	if v := d.Decide(context.Background(), other); !v.Allowed {
		t.Fatalf("another principal = %+v, want its own budget", v)
	}
}

func TestDecideD9OperatorApprovedNotLimited(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.MaxRequestsPerHour = 1
	cfg.Pending = func(context.Context, string) (int, error) { return 99, nil }
	d := New(cfg)
	req := proposeMigration()
	req.OperatorApproved = true
	for range 3 {
		if v := d.Decide(context.Background(), req); !v.Allowed {
			t.Fatalf("approved = %+v, want D9 not to bind", v)
		}
	}
}

func TestDecideD10Lease(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.Leases = fakeLeases{active: false}
	wantDenied(t, decideWith(t, cfg, agentRead()), ReasonLeaseExpired, "D10")
	cfg.Leases = nil
	wantDenied(t, decideWith(t, cfg, agentRead()), ReasonLeaseExpired, "D10")
	cfg.Leases = fakeLeases{err: errBoom}
	wantDenied(t, decideWith(t, cfg, agentRead()), ReasonUnavailable, "D10")
	req := agentRead()
	req.GrantID = ""
	cfg.Leases = nil
	wantAllowed(t, decideWith(t, cfg, req), 3)
}

func TestDecideFirstFailureWins(t *testing.T) {
	// Frozen (D1) and over the ceiling (D3) and lease expired (D10): D1.
	p := activePrincipal()
	p.Status = agentguard.StatusFrozen
	p.EnvCeiling = agentguard.EnvBranch
	cfg, _ := fullConfig(p)
	cfg.Leases = fakeLeases{}
	wantDenied(t, decideWith(t, cfg, agentRead()), agentguard.ReasonFrozen, "D1")
	// An expired lease never reports agent_level0 (§6.2.3).
	cfg, _ = fullConfig(activePrincipal())
	cfg.Leases = fakeLeases{}
	if v := decideWith(t, cfg, agentRead()); v.Reason == agentguard.ReasonLevel0 {
		t.Fatalf("verdict = %+v, the D-step reason must win", v)
	}
}

func TestDecideNarrowingSkipsDSteps(t *testing.T) {
	p := activePrincipal()
	p.Status, p.SponsorUserID = agentguard.StatusFrozen, nil
	cfg, _ := fullConfig(p)
	cfg.Leases = nil
	req := agentRead()
	req.Narrowing = true
	v := decideWith(t, cfg, req)
	if !v.Allowed || v.Reason != "" {
		t.Fatalf("narrowing = %+v, want allowed: containment is never blocked", v)
	}
}

func TestDecideConcurrentRateIsExact(t *testing.T) {
	cfg, _ := fullConfig(activePrincipal())
	cfg.MaxRequestsPerHour = 10
	d := New(cfg)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d.Decide(context.Background(), proposeMigration()).Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("allowed = %d of 50 concurrent requests, want exactly 10", allowed)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	d := New(Config{})
	if d.cfg.MaxRequestsPerHour != 30 || d.cfg.MaxPendingPerPrincipal != 10 ||
		d.cfg.RestoreDrillDays != 14 || d.cfg.Now == nil {
		t.Fatalf("defaults = %+v", d.cfg)
	}
	d = New(Config{MaxRequestsPerHour: -1, MaxPendingPerPrincipal: 1, RestoreDrillDays: 1})
	if d.cfg.MaxRequestsPerHour != 30 || d.cfg.MaxPendingPerPrincipal != 1 ||
		d.cfg.RestoreDrillDays != 1 {
		t.Fatalf("explicit = %+v", d.cfg)
	}
}

func TestCapabilityPredicates(t *testing.T) {
	for _, c := range []Capability{CapRead, CapWriteInsert, CapWriteUpdate, CapWriteDelete,
		CapDDLAdditive, CapDDLLocking, CapDDLDestructive, CapMaint, CapSandbox,
		CapPolicyProposal} {
		if !c.Valid() {
			t.Fatalf("%s must be valid", c)
		}
		want := fmt.Sprint(strings.HasPrefix(string(c), "write_") ||
			strings.HasPrefix(string(c), "ddl_") || c == CapMaint)
		if fmt.Sprint(c.Mutates()) != want {
			t.Fatalf("%s.Mutates() = %v", c, c.Mutates())
		}
	}
	if Capability("").Valid() || Capability("READ").Valid() {
		t.Fatal("unknown classes are invalid")
	}
}
