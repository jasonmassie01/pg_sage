package envbind

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Spec §5.3 and §6.5: four labels, prod by default; a label is bound to a
// physical identity tuple and the agent's claim is never an input.

func TestParseEnv(t *testing.T) {
	for _, s := range []string{"branch", "dev", "stage", "prod"} {
		e, err := ParseEnv(s)
		if err != nil || string(e) != s {
			t.Errorf("ParseEnv(%q) = %q, %v", s, e, err)
		}
	}
	for _, s := range []string{"", "PROD", "production", "qa", " dev", "dev\x00"} {
		e, err := ParseEnv(s)
		if !errors.Is(err, ErrInvalidEnv) || e != "" {
			t.Errorf("ParseEnv(%q) = %q, %v; want ErrInvalidEnv", s, e, err)
		}
	}
}

func TestEnvOrder(t *testing.T) {
	order := []Env{EnvBranch, EnvDev, EnvStage, EnvProd}
	for i, e := range order {
		if e.Rank() != i {
			t.Errorf("%s rank %d, want %d", e, e.Rank(), i)
		}
	}
	if Env("qa").Rank() != EnvProd.Rank() {
		t.Error("an unknown label must rank as prod (fail closed)")
	}
	cases := []struct {
		env, ceiling Env
		within       bool
	}{
		{EnvBranch, EnvDev, true}, {EnvDev, EnvDev, true}, {EnvStage, EnvDev, false},
		{EnvProd, EnvStage, false}, {EnvProd, EnvProd, true}, {Env("qa"), EnvStage, false},
	}
	for _, c := range cases {
		if got := c.env.Within(c.ceiling); got != c.within {
			t.Errorf("%s within %s = %v", c.env, c.ceiling, got)
		}
	}
	if !Widens(EnvProd, EnvDev) || Widens(EnvDev, EnvProd) || Widens(EnvDev, EnvDev) {
		t.Error("Widens: prod->dev widens; dev->prod and dev->dev do not")
	}
}

func TestIdentityStrength(t *testing.T) {
	cases := []struct {
		id   Identity
		want Strength
	}{
		{Identity{}, StrengthConfigured},
		{Identity{Target: "h:5432/db"}, StrengthConfigured},
		{Identity{ProviderRef: "rds:db-1"}, StrengthProvider},
		{Identity{SystemIdentifier: "7", DBOID: 5}, StrengthCluster},
		{Identity{SystemIdentifier: "7"}, StrengthConfigured},
		{Identity{ProviderRef: "rds:x", SystemIdentifier: "7", DBOID: 5}, StrengthCluster},
	}
	for _, c := range cases {
		if got := c.id.Strength(); got != c.want {
			t.Errorf("%+v strength %s, want %s", c.id, got, c.want)
		}
	}
	if (Identity{SystemIdentifier: "7"}).PhysicalKey() != "" {
		t.Error("no oid means no physical key")
	}
	if got := (Identity{SystemIdentifier: "7", DBOID: 5}).PhysicalKey(); got != "7/5" {
		t.Errorf("physical key %q", got)
	}
}

func TestIdentityDiff(t *testing.T) {
	base := Identity{ProviderRef: "rds:a", SystemIdentifier: "7", DBOID: 5,
		Target: "h:5432/app", InRecovery: false}
	if d := base.Diff(base); len(d) != 0 {
		t.Fatalf("same identity differs: %v", d)
	}
	recovering := base
	recovering.InRecovery = true
	if d := base.Diff(recovering); len(d) != 0 {
		t.Errorf("recovery state alone is evidence, not a binding change: %v", d)
	}
	for field, mut := range map[string]func(*Identity){
		"provider_ref":      func(i *Identity) { i.ProviderRef = "rds:b" },
		"system_identifier": func(i *Identity) { i.SystemIdentifier = "8" },
		"db_oid":            func(i *Identity) { i.DBOID = 6 },
		"target":            func(i *Identity) { i.Target = "prod:5432/app" },
	} {
		other := base
		mut(&other)
		if d := base.Diff(other); !slices.Equal(d, []string{field}) {
			t.Errorf("%s change: diff %v", field, d)
		}
	}
	unreadable := base
	unreadable.SystemIdentifier = ""
	if d := base.Diff(unreadable); !slices.Contains(d, "system_identifier") {
		t.Error("a system identifier that became unreadable is a change (fail closed)")
	}
}

func row(label Env, id Identity, verified bool) *LabelRecord {
	return &LabelRecord{DatabaseID: "db-a", Label: label, Identity: id, Verified: verified,
		SetBy: "admin@example.com", SetAt: time.Unix(1_700_000_000, 0)}
}

var devID = Identity{SystemIdentifier: "100", DBOID: 16384, Target: "dev:5432/app"}

func TestEvaluate_DefaultsToProd(t *testing.T) {
	ev := evaluate(evalInput{Live: devID})
	if ev.Effective != EnvProd || ev.Label != EnvProd || ev.Verified ||
		!slices.Equal(ev.Reasons, []string{ReasonDefaultProd}) || ev.Critical {
		t.Fatalf("no label row: %+v", ev)
	}
	ev = evaluate(evalInput{Row: row(EnvProd, devID, false), Live: devID})
	if ev.Effective != EnvProd || !slices.Equal(ev.Reasons, []string{ReasonLabelProd}) {
		t.Fatalf("prod label: %+v", ev)
	}
}

func TestEvaluate_VerifiedDev(t *testing.T) {
	ev := evaluate(evalInput{Row: row(EnvDev, devID, true), Live: devID})
	if ev.Effective != EnvDev || !ev.Verified || ev.Critical ||
		!slices.Equal(ev.Reasons, []string{ReasonVerified}) {
		t.Fatalf("verified dev: %+v", ev)
	}
	if ev.Strength != StrengthCluster {
		t.Errorf("strength %s", ev.Strength)
	}
}

func TestEvaluate_UnverifiedLabelIsProd(t *testing.T) {
	ev := evaluate(evalInput{Row: row(EnvDev, devID, false), Live: devID})
	if ev.Effective != EnvProd || ev.Verified ||
		!slices.Contains(ev.Reasons, ReasonLabelUnverified) {
		t.Fatalf("unverified dev label: %+v", ev)
	}
}

func TestEvaluate_ConfiguredStrengthIsProd(t *testing.T) {
	weak := Identity{Target: "dev:5432/app"}
	ev := evaluate(evalInput{Row: row(EnvStage, weak, true), Live: weak})
	if ev.Effective != EnvProd || !slices.Contains(ev.Reasons, ReasonUnverifiable) {
		t.Fatalf("host:port alone never verifies a binding: %+v", ev)
	}
}

func TestEvaluate_RepointedDSN(t *testing.T) {
	live := devID
	live.Target = "prod-primary:5432/app"
	live.SystemIdentifier, live.DBOID = "999", 16390
	ev := evaluate(evalInput{Row: row(EnvDev, devID, true), Live: live})
	if ev.Effective != EnvProd || !ev.Critical ||
		!slices.Contains(ev.Reasons, ReasonBindingChanged) {
		t.Fatalf("re-pointed DSN: %+v", ev)
	}
	if !slices.Equal(ev.Changed, []string{"system_identifier", "db_oid", "target"}) {
		t.Errorf("changed fields %v", ev.Changed)
	}
}

func TestEvaluate_TwoLabelsSamePhysicalIdentity(t *testing.T) {
	// A pg_basebackup copy or standby of prod: same system identifier and
	// database OID, registered under another database_id as dev.
	prodPeer := Peer{DatabaseID: "db-prod", Label: EnvProd,
		Identity: Identity{SystemIdentifier: "100", DBOID: 16384, Target: "prod:5432/app"}}
	ev := evaluate(evalInput{Row: row(EnvDev, devID, true), Live: devID,
		Peers: []Peer{prodPeer}})
	if ev.Effective != EnvProd || !ev.Critical ||
		!slices.Contains(ev.Reasons, ReasonTwoLabels) || len(ev.Conflicts) != 1 ||
		ev.Conflicts[0].DatabaseID != "db-prod" {
		t.Fatalf("clone of prod as dev: %+v", ev)
	}
	// The prod side is prod either way, but the conflict is critical there too.
	devPeer := Peer{DatabaseID: "db-a", Label: EnvDev, Identity: devID}
	pv := evaluate(evalInput{Row: row(EnvProd, prodPeer.Identity, false),
		Live: prodPeer.Identity, Peers: []Peer{devPeer}})
	if pv.Effective != EnvProd || !pv.Critical || !slices.Contains(pv.Reasons, ReasonTwoLabels) {
		t.Fatalf("prod side of a two-label conflict: %+v", pv)
	}
	// Same label twice is not a conflict.
	same := evaluate(evalInput{Row: row(EnvDev, devID, true), Live: devID,
		Peers: []Peer{{DatabaseID: "db-b", Label: EnvDev, Identity: devID}}})
	if same.Effective != EnvDev || same.Critical {
		t.Fatalf("one physical database twice under dev: %+v", same)
	}
}

func TestEvaluate_DistinguishingProviderRef(t *testing.T) {
	a := devID
	a.ProviderRef, a.RefSource = "rds:dev-1", RefCloudTel
	b := Peer{DatabaseID: "db-prod", Label: EnvProd, Identity: Identity{
		SystemIdentifier: "100", DBOID: 16384, ProviderRef: "rds:prod-1", RefSource: RefCloudTel}}
	ev := evaluate(evalInput{Row: row(EnvDev, a, true), Live: a, Peers: []Peer{b}})
	if ev.Effective != EnvDev || ev.Critical {
		t.Fatalf("two provider ids distinguish one system identifier: %+v", ev)
	}
	b.Identity.ProviderRef = ""
	ev = evaluate(evalInput{Row: row(EnvDev, a, true), Live: a, Peers: []Peer{b}})
	if ev.Effective != EnvProd || !slices.Contains(ev.Reasons, ReasonTwoLabels) {
		t.Fatalf("a configured provider id on one side does not distinguish: %+v", ev)
	}
}

func receipt(name, ref string) *clone.Receipt {
	return &clone.Receipt{ID: 1, Adapter: "dle", Scope: "s", Ref: ref, Name: name,
		Purpose: "sandbox", Status: clone.StatusReady,
		ExpiresAt: time.Now().Add(time.Hour)}
}

func TestEvaluate_BranchNeedsReceipt(t *testing.T) {
	ev := evaluate(evalInput{Row: row(EnvBranch, devID, true), Live: devID})
	if ev.Effective != EnvProd || !ev.Critical ||
		!slices.Contains(ev.Reasons, ReasonReceiptMissing) {
		t.Fatalf("branch without a receipt: %+v", ev)
	}
	live := devID
	live.ProviderRef, live.RefSource = "dle:clone-7", RefReceipt
	snap := live
	prodPeer := Peer{DatabaseID: "db-prod", Label: EnvProd,
		Identity: Identity{SystemIdentifier: "100", DBOID: 16384}}
	ev = evaluate(evalInput{Row: row(EnvBranch, snap, true), Live: live,
		Receipt: receipt("sbx", "clone-7"), Peers: []Peer{prodPeer}})
	if ev.Effective != EnvBranch || ev.Critical || ev.Receipt == nil {
		t.Fatalf("receipt-backed DBLab clone of prod: %+v", ev)
	}
}

func TestEvaluate_ReceiptMismatch(t *testing.T) {
	live := devID
	live.ProviderRef, live.RefSource = "neon:br-1", RefCloudTel
	ev := evaluate(evalInput{Row: row(EnvBranch, live, true), Live: live,
		Receipt: receipt("sbx", "clone-7")})
	if ev.Effective != EnvProd || !slices.Contains(ev.Reasons, ReasonReceiptMismatch) {
		t.Fatalf("receipt for another resource: %+v", ev)
	}
}

func TestCheckLabel(t *testing.T) {
	cases := []struct {
		name   string
		label  Env
		live   Identity
		peers  []Peer
		rcpt   *clone.Receipt
		reason string
	}{
		{"prod always allowed", EnvProd, Identity{}, nil, nil, ""},
		{"dev on cluster identity", EnvDev, devID, nil, nil, ""},
		{"dev on host:port only", EnvDev, Identity{Target: "x"}, nil, nil, ReasonUnverifiable},
		{"branch without receipt", EnvBranch, devID, nil, nil, ReasonReceiptMissing},
		{"dev clone of prod", EnvDev, devID, []Peer{{DatabaseID: "p", Label: EnvProd,
			Identity: devID}}, nil, ReasonTwoLabels},
	}
	for _, c := range cases {
		err := checkLabel(c.label, c.live, c.peers, c.rcpt)
		if c.reason == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Reason != c.reason ||
			!errors.Is(err, ErrLabelRefused) {
			t.Errorf("%s: %v, want refusal %s", c.name, err, c.reason)
		}
	}
}
