package broker

import (
	"context"
	"errors"
	"testing"
)

// These paths are decided before the statement is parsed or a connection
// is opened, so they need neither cgo nor a server.

func newFakeBroker(t *testing.T, deps Deps) *Broker {
	t.Helper()
	b, err := New(DefaultConfig(), deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

func TestQueryWithoutPrincipalIsUnsponsored(t *testing.T) {
	deps := fakeDeps()
	audit := deps.Audit.(*memAudit)
	b := newFakeBroker(t, deps)
	res, err := b.Query(context.Background(), Request{Database: "db", SQL: "SELECT 1"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Verdict != VerdictBlocked || res.ReasonCode != "agent_unsponsored" {
		t.Errorf("result = %+v, want blocked agent_unsponsored", res)
	}
	if res.Fix == "" {
		t.Error("the refusal does not say how to get a principal")
	}
	if len(audit.all()) != 0 {
		t.Error("a call with no principal wrote an audit row it cannot attribute")
	}
	if deps.Logins.(*fakeLogins).calls != 0 {
		t.Error("a broker login was opened for a caller with no principal")
	}
}

func TestQueryOutsideTokenDatabasesIsNotPermitted(t *testing.T) {
	b := newFakeBroker(t, fakeDeps())
	ctx := withAgent(context.Background(), testPrincipal(), []string{"other"})
	_, err := b.Query(ctx, Request{Database: "db", SQL: "SELECT 1"})
	if !errors.Is(err, ErrNotPermitted) {
		t.Errorf("error = %v, want ErrNotPermitted", err)
	}
}

func TestQueryUnknownDatabase(t *testing.T) {
	b := newFakeBroker(t, fakeDeps())
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	_, err := b.Query(ctx, Request{Database: "nope", SQL: "SELECT 1"})
	if !errors.Is(err, ErrUnknownDatabase) {
		t.Errorf("error = %v, want ErrUnknownDatabase", err)
	}
}

func TestQueryInvalidArguments(t *testing.T) {
	b := newFakeBroker(t, fakeDeps())
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	_, err := b.Query(ctx, Request{Database: "db", SQL: "SELECT 1", MaxRows: 100000})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
}

func TestQueryUnboundDatabaseIsBlocked(t *testing.T) {
	deps := fakeDeps()
	deps.Targets = fakeTargets{"db": {Name: "db"}} // no database_id
	b := newFakeBroker(t, deps)
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	res, err := b.Query(ctx, Request{Database: "db", SQL: "SELECT 1"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Verdict != VerdictBlocked || res.ReasonCode != ReasonDatabaseUnbound {
		t.Errorf("result = %+v, want blocked %s", res, ReasonDatabaseUnbound)
	}
}

func TestQueryForbiddenCharactersBlockedBeforeParsing(t *testing.T) {
	deps := fakeDeps()
	deps.Targets = fakeTargets{"db": {Name: "db", DatabaseID: "00000000-0000-0000-0000-" +
		"000000000001"}}
	audit := deps.Audit.(*memAudit)
	b := newFakeBroker(t, deps)
	ctx := withAgent(context.Background(), testPrincipal(), nil)
	res, err := b.Query(ctx, Request{Database: "db", SQL: "SELECT 1 \u202E-- x"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.ReasonCode != ReasonForbiddenCharacters {
		t.Errorf("result = %+v, want %s", res, ReasonForbiddenCharacters)
	}
	recs := audit.all()
	if len(recs) != 1 || recs[0].Verdict != VerdictBlocked ||
		recs[0].Reason != ReasonForbiddenCharacters || recs[0].TaskID != "task-1" {
		t.Errorf("audit = %+v, want one blocked row with the task", recs)
	}
}
