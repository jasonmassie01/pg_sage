package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// security-09b: a meta-DB member's stored action credential becomes its
// action pool; a credential no key opens leaves observation running and
// every change failing closed, never on the observation role.

const obsMemberDSN = "postgres://observer:obs-pw@db.example.com:5432/app?sslmode=require"

func TestClassifyMemberAction(t *testing.T) {
	act := "postgres://actor:act-pw@db.example.com:5432/app?sslmode=require"
	got, err := classifyMemberAction(act, nil, "actor", obsMemberDSN)
	if err != nil || got.dsn != act || got.err != nil {
		t.Fatalf("readable credential = %+v, %v", got, err)
	}
	if got, err := classifyMemberAction("", nil, "", obsMemberDSN); err != nil ||
		got != (memberAction{}) {
		t.Fatalf("no credential = %+v, %v; want the zero value", got, err)
	}
	wrong := fmt.Errorf("database 9: action password: %w", crypto.ErrWrongEncryptionKey)
	got, err = classifyMemberAction("", wrong, "actor", obsMemberDSN)
	if err != nil || !errors.Is(got.err, crypto.ErrWrongEncryptionKey) {
		t.Fatalf("wrong key = %+v, %v; want a fail-closed action", got, err)
	}
	u, perr := url.Parse(got.dsn)
	if perr != nil || u.User.Username() != "actor" || u.Host != "db.example.com:5432" ||
		u.Path != "/app" {
		t.Fatalf("fail-closed DSN %q does not name the action role on the member", got.dsn)
	}
	if _, set := u.User.Password(); set || strings.Contains(got.dsn, "obs-pw") {
		t.Fatalf("fail-closed DSN carries a password: %q", got.dsn)
	}
	boom := errors.New("connection refused")
	if _, err := classifyMemberAction("", boom, "actor", obsMemberDSN); !errors.Is(err, boom) {
		t.Fatalf("a read failure = %v, want it returned", err)
	}
}

// Error propagation: the fail-closed pool never connects; its ping names
// the database and both sentinels, and the executor would report "action
// identity unavailable".
func TestUnavailableActionPool_RefusesEveryConnection(t *testing.T) {
	wrong := fmt.Errorf("database 9: action password: %w", crypto.ErrWrongEncryptionKey)
	a, err := classifyMemberAction("", wrong, "actor", obsMemberDSN)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.open(obsMemberDSN, "orders")
	if err != nil || p == nil {
		t.Fatalf("open = %v, %v; want a refusing pool", p, err)
	}
	defer p.Close()
	if p.Config().ConnConfig.User != "actor" || p.Config().ConnConfig.Password != "" {
		t.Fatalf("pool user %q password set %t", p.Config().ConnConfig.User,
			p.Config().ConnConfig.Password != "")
	}
	err = p.Ping(context.Background())
	if !errors.Is(err, executor.ErrActionIdentityUnavailable) ||
		!errors.Is(err, crypto.ErrWrongEncryptionKey) ||
		!strings.Contains(err.Error(), "database 9") || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("ping = %v, want unavailable, wrong key, naming database 9 and orders", err)
	}
	if runningActionDSN(&fleet.DatabaseInstance{ActionPool: p}) != a.dsn {
		t.Fatal("the running key of a fail-closed pool is not its classified DSN")
	}
}

// Nil/empty: no credential opens no pool; a nil instance or one without an
// action pool has an empty running key.
func TestMemberAction_NoneOpensNothing(t *testing.T) {
	p, err := memberAction{}.open(obsMemberDSN, "orders")
	if p != nil || err != nil {
		t.Fatalf("open(no credential) = %v, %v", p, err)
	}
	if runningActionDSN(nil) != "" || runningActionDSN(&fleet.DatabaseInstance{}) != "" {
		t.Fatal("an instance without an action pool has a running action DSN")
	}
}

// Invalid input: a readable credential that names another database is
// refused by the same validation as YAML action_dsn.
func TestMemberAction_RefusesAnotherDatabase(t *testing.T) {
	a := memberAction{dsn: "postgres://actor:pw@db.example.com:5432/billing?sslmode=require"}
	if p, err := a.open(obsMemberDSN, "orders"); p != nil || err == nil {
		t.Fatalf("open(another database) = %v, %v; want refused", p, err)
	}
}

// State transitions: the prospective record of an update carries the
// action role it will run with.
func TestUpdatedDatabaseRecord_ActionUsername(t *testing.T) {
	old := store.DatabaseRecord{Name: "m", ActionUsername: "actor"}
	cases := []struct {
		name   string
		action *store.ActionCredentialInput
		want   string
	}{
		{"nil keeps", nil, "actor"},
		{"empty removes", &store.ActionCredentialInput{}, ""},
		{"rename", &store.ActionCredentialInput{Username: "actor2"}, "actor2"},
	}
	for _, tc := range cases {
		got := updatedDatabaseRecord(old, store.DatabaseInput{Name: "m", Action: tc.action})
		if got.ActionUsername != tc.want {
			t.Errorf("%s: ActionUsername = %q, want %q", tc.name, got.ActionUsername, tc.want)
		}
	}
}

// connected is a running generation with lazy pools (MinConns 0, so
// nothing dials): an observation pool and, when actionDSN is set, an
// action pool.
func connected(t *testing.T, actionDSN string) *fleet.DatabaseInstance {
	t.Helper()
	inst := &fleet.DatabaseInstance{Pool: lazyDSNPool(t, obsMemberDSN)}
	if actionDSN != "" {
		inst.ActionPool = lazyDSNPool(t, actionDSN)
	}
	return inst
}

func lazyDSNPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestMetaActionChanged(t *testing.T) {
	const actA = "postgres://actor:a@db.example.com:5432/app?sslmode=require"
	const actB = "postgres://actor:b@db.example.com:5432/app?sslmode=require"
	running := &fleet.DatabaseInstance{}
	cases := []struct {
		name    string
		current *fleet.DatabaseInstance
		action  memberAction
		want    bool
	}{
		{"absent member", nil, memberAction{dsn: "x"}, false},
		{"failed placeholder", &fleet.DatabaseInstance{}, memberAction{dsn: "x"}, false},
		{"none on either side", running, memberAction{}, false},
		{"connected, none to one", connected(t, ""), memberAction{dsn: actA}, true},
		{"same identity", connected(t, actA), memberAction{dsn: actA}, false},
		{"changed identity", connected(t, actA), memberAction{dsn: actB}, true},
		{"removed identity", connected(t, actA), memberAction{}, true},
	}
	for _, tc := range cases {
		if got := metaActionChanged(tc.current, tc.action); got != tc.want {
			t.Errorf("%s: metaActionChanged = %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestResealStoredPasswords_ReportsActionPasswordsSeparately(t *testing.T) {
	var lines []string
	logf := func(level, component, msg string, args ...any) {
		lines = append(lines, fmt.Sprintf(level+" "+msg, args...))
	}
	keyless := store.NewDatabaseStore(nil, nil)
	if err := resealStoredPasswords(context.Background(), keyless, logf); err != nil {
		t.Fatalf("keyless reseal: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("keyless reseal logged %v", lines)
	}
}
