package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// fleetpol-03: the sprawl census's TTL proposals reach GET /api/v1/inbox
// as advice: every proposal for an unscoped caller (observe-only members
// have no runtime), only the visible databases' for a scoped one.

type inboxCensus struct {
	props []fleet.CensusProposal
	err   error
}

func (c inboxCensus) CensusProposals(context.Context) ([]fleet.CensusProposal, error) {
	return c.props, c.err
}

func censusFleet(c inboxCensus, names ...string) *fleet.DatabaseManager {
	mgr := offlineFleet(names...)
	mgr.SetCensus(c)
	return mgr
}

func adviceProps() []fleet.CensusProposal {
	at := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	return []fleet.CensusProposal{
		{RunID: 2, RunAt: at, DatabaseKey: "orders",
			Proposal: "no real activity in 40 days: propose TTL 7 days"},
		{RunID: 2, RunAt: at, DatabaseKey: "branch-obs",
			Proposal: "no real activity in 31 days: propose TTL 7 days"},
	}
}

func TestInboxRoutes_CensusAdviceForEveryDatabase(t *testing.T) {
	h := inboxMux(censusFleet(inboxCensus{props: adviceProps()}, "orders"), nil, nil)
	code, out, body := inboxGet(t, h, "/api/v1/inbox?kinds=advice", testViewerUser())
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 2 || out["lower_bound"] != false ||
		!strings.Contains(body, `"database":"branch-obs"`) ||
		!strings.Contains(body, `"kind":"advice"`) {
		t.Fatalf("advice = %d %s, want both proposals", code, body)
	}
	first := items[0].(map[string]any)
	if first["actionable"] != false || first["risk"] != "safe" {
		t.Fatalf("a viewer's advice item %v, want safe and not actionable", first)
	}
	_, out, body = inboxGet(t, h, "/api/v1/inbox?kinds=advice&database=orders",
		testViewerUser())
	if items, _ := out["items"].([]any); len(items) != 1 ||
		strings.Contains(body, "branch-obs") {
		t.Fatalf("one database: %s, want orders' advice only", body)
	}
}

func TestInboxRoutes_ScopedCallerSeesOnlyItsDatabasesAdvice(t *testing.T) {
	src := &dbAuthzSource{}
	src.set(2, map[string]string{"orders": auth.RoleOperator})
	h := inboxMux(censusFleet(inboxCensus{props: adviceProps()}, "orders", "billing"),
		nil, src)
	code, out, body := inboxGet(t, h, "/api/v1/inbox?kinds=advice", dbOperator(2))
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 || strings.Contains(body, "branch-obs") {
		t.Fatalf("scoped advice = %d %s, want orders' only", code, body)
	}
	if items[0].(map[string]any)["actionable"] != true {
		t.Fatalf("an operator's advice on orders %v, want actionable", items[0])
	}
}

func TestInboxRoutes_FailingCensusIsALowerBound(t *testing.T) {
	h := inboxMux(censusFleet(inboxCensus{err: errors.New("meta down")}, "orders"),
		nil, nil)
	_, out, body := inboxGet(t, h, "/api/v1/inbox/count?kinds=advice", testViewerUser())
	if out["lower_bound"] != true || !strings.Contains(body, `"source":"advice"`) ||
		strings.Contains(body, "meta down") {
		t.Fatalf("failing census count = %s, want a named, redacted lower bound", body)
	}
}
