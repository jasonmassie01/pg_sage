package policy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

var (
	policySchemaOnce sync.Once
	policySchemaErr  error
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/policy"))
}

func TestBootstrapCreatesProfileOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	first, err := store.Bootstrap(ctx, Scope{}, "unattended", "bootstrap")
	require.NoError(t, err)
	require.Equal(t, int64(1), first.Version)
	require.Equal(t, "unattended", first.Profile)
	require.Equal(t, "bootstrap", first.UpdatedBy)
	require.JSONEq(t, profileJSON(t, UnattendedProfile()), string(first.Document))

	second, err := store.Bootstrap(ctx, Scope{}, "staffed", "other-actor")
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.Version, second.Version)
	require.Equal(t, "unattended", second.Profile)
	require.JSONEq(t, string(first.Document), string(second.Document))
}

func TestProposeDoesNotChangeCurrentPolicyUntilRatified(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	current := bootstrapPolicy(t, store, Scope{})

	proposal, err := store.Propose(ctx, ProposalRequest{
		Scope:           Scope{},
		ExpectedVersion: current.Version,
		Profile:         "staffed",
		Document:        json.RawMessage(staffedDocument()),
		Actor:           "operator@test.com",
		Preview: ImpactPreview{ChangedPendingOutcomes: []PendingOutcomeChange{
			{ActionID: 17, Before: "blocked", After: "execute"},
		}},
	})
	require.NoError(t, err)
	require.Equal(t, current.Version, proposal.BaseVersion)
	require.Nil(t, proposal.RatifiedAt)
	require.Len(t, proposal.Preview.ChangedPendingOutcomes, 1)

	unchanged, err := store.Current(ctx, Scope{})
	require.NoError(t, err)
	require.Equal(t, current.ID, unchanged.ID)
	require.Equal(t, int64(1), unchanged.Version)
	require.Equal(t, "unattended", unchanged.Profile)

	ratified, err := store.Ratify(ctx, RatifyRequest{
		ProposalID:      proposal.ID,
		ExpectedVersion: current.Version,
		Actor:           "admin@test.com",
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), ratified.Version)
	require.Equal(t, "staffed", ratified.Profile)
	require.Equal(t, "admin@test.com", ratified.UpdatedBy)
	require.JSONEq(t, staffedDocument(), string(ratified.Document))

	history, err := store.History(ctx, Scope{}, 10)
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, int64(2), history[0].Version)
	require.Equal(t, int64(1), history[1].Version)
	require.Equal(t, current.ID, history[1].ID)
	require.JSONEq(t, profileJSON(t, UnattendedProfile()), string(history[1].Document))
}

func TestRatifyRejectsStaleExpectedVersion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	current := bootstrapPolicy(t, store, Scope{})
	first := proposePolicy(t, store, Scope{}, current.Version, "staffed")
	second := proposePolicy(t, store, Scope{}, current.Version, "staffed")

	_, err := store.Ratify(ctx, RatifyRequest{
		ProposalID: first.ID, ExpectedVersion: current.Version, Actor: "admin-a",
	})
	require.NoError(t, err)

	_, err = store.Ratify(ctx, RatifyRequest{
		ProposalID: second.ID, ExpectedVersion: current.Version, Actor: "admin-b",
	})
	require.ErrorIs(t, err, ErrVersionConflict)

	after, err := store.Current(ctx, Scope{})
	require.NoError(t, err)
	require.Equal(t, int64(2), after.Version)
	require.Equal(t, "admin-a", after.UpdatedBy)
}

func TestConcurrentRatificationHasSingleWinner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	current := bootstrapPolicy(t, store, Scope{})
	proposals := []Proposal{
		proposePolicy(t, store, Scope{}, current.Version, "staffed"),
		proposePolicy(t, store, Scope{}, current.Version, "staffed"),
	}

	start := make(chan struct{})
	errs := make(chan error, len(proposals))
	var wg sync.WaitGroup
	for i, proposal := range proposals {
		wg.Add(1)
		go func(index int, candidate Proposal) {
			defer wg.Done()
			<-start
			_, err := store.Ratify(ctx, RatifyRequest{
				ProposalID: candidate.ID, ExpectedVersion: current.Version,
				Actor: "admin-concurrent-" + string(rune('a'+index)),
			})
			errs <- err
		}(i, proposal)
	}
	close(start)
	wg.Wait()
	close(errs)

	var succeeded, conflicted int
	for err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrVersionConflict):
			conflicted++
		default:
			t.Fatalf("unexpected ratification error: %v", err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, conflicted)

	history, err := store.History(ctx, Scope{}, 10)
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, int64(2), history[0].Version)
}

func TestPolicyScopeIsIndependentPerDatabase(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	global := bootstrapPolicy(t, store, Scope{})
	db41 := bootstrapPolicy(t, store, Scope{DatabaseID: storeInt64Pointer(41)})
	db42 := bootstrapPolicy(t, store, Scope{DatabaseID: storeInt64Pointer(42)})

	proposal := proposePolicy(t, store, db41.Scope, db41.Version, "staffed")
	updated41, err := store.Ratify(ctx, RatifyRequest{
		ProposalID: proposal.ID, ExpectedVersion: db41.Version, Actor: "admin",
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), updated41.Version)

	unchangedGlobal, err := store.Current(ctx, Scope{})
	require.NoError(t, err)
	unchanged42, err := store.Current(ctx, db42.Scope)
	require.NoError(t, err)
	require.Equal(t, global.ID, unchangedGlobal.ID)
	require.Equal(t, int64(1), unchangedGlobal.Version)
	require.Equal(t, db42.ID, unchanged42.ID)
	require.Equal(t, int64(1), unchanged42.Version)
	require.NotEqual(t, updated41.ID, unchanged42.ID)
}

func TestDocumentValidationPreservesNullAndZeroBudgets(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	current := bootstrapPolicy(t, store, Scope{})

	proposal, err := store.Propose(ctx, ProposalRequest{
		Scope: Scope{}, ExpectedVersion: current.Version, Profile: "staffed",
		Document: json.RawMessage(staffedDocument()), Actor: "operator",
	})
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, json.Unmarshal(proposal.Document, &document))
	budgets := document["budgets"].(map[string]any)
	require.Equal(t, float64(0), budgets["storage_bytes"])
	require.Nil(t, budgets["spend_daily"])
	require.Equal(t, float64(500000), budgets["llm_tokens_daily"])
}

func TestDocumentValidationRejectsInvalidDocuments(t *testing.T) {
	testCases := []struct {
		name     string
		document string
	}{
		{name: "invalid JSON", document: `{`},
		{name: "JSON null", document: `null`},
		{name: "missing budgets", document: `{
			"unknown_classification":"fail_closed","windows":["02:00-04:00"]}`},
		{name: "negative budget", document: `{
			"unknown_classification":"fail_closed","windows":["02:00-04:00"],
			"budgets":{"storage_bytes":-1,"spend_daily":null,"llm_tokens_daily":5}}`},
		{name: "unsafe unknown classification", document: `{
			"unknown_classification":"allow","windows":["02:00-04:00"],
			"budgets":{"storage_bytes":0,"spend_daily":null,"llm_tokens_daily":5}}`},
		{name: "invalid window", document: `{
			"unknown_classification":"fail_closed","windows":["not-a-window"],
			"budgets":{"storage_bytes":0,"spend_daily":null,"llm_tokens_daily":5}}`},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			store := newTestStore(t)
			current := bootstrapPolicy(t, store, Scope{})
			_, err := store.Propose(context.Background(), ProposalRequest{
				Scope: Scope{}, ExpectedVersion: current.Version, Profile: "staffed",
				Document: json.RawMessage(testCase.document), Actor: "operator",
			})
			require.ErrorIs(t, err, ErrInvalidDocument)
		})
	}
}

func TestStorePropagatesCancellationAndUnavailablePool(t *testing.T) {
	store := newTestStore(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.Current(canceled, Scope{})
	require.ErrorIs(t, err, context.Canceled)

	unavailable := NewStore(nil)
	_, err = unavailable.Current(context.Background(), Scope{})
	require.ErrorIs(t, err, ErrUnavailable)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	store := NewStore(pool)
	policySchemaOnce.Do(func() {
		policySchemaErr = schema.Bootstrap(context.Background(), pool)
	})
	require.NoError(t, policySchemaErr)
	_, err = pool.Exec(context.Background(), `
		TRUNCATE sage.policy RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return store
}

func bootstrapPolicy(t *testing.T, store *Store, scope Scope) Policy {
	t.Helper()
	result, err := store.Bootstrap(context.Background(), scope, "unattended", "bootstrap")
	require.NoError(t, err)
	return result
}

func proposePolicy(
	t *testing.T, store *Store, scope Scope, expectedVersion int64, profile string,
) Proposal {
	t.Helper()
	result, err := store.Propose(context.Background(), ProposalRequest{
		Scope: scope, ExpectedVersion: expectedVersion, Profile: profile,
		Document: json.RawMessage(staffedDocument()), Actor: "operator",
	})
	require.NoError(t, err)
	return result
}

func storeInt64Pointer(value int64) *int64 {
	return &value
}

func profileJSON(t *testing.T, document Document) string {
	t.Helper()
	raw, err := MarshalDocument(document)
	require.NoError(t, err)
	return string(raw)
}

func staffedDocument() string {
	return `{
		"allowed_change_classes":["index","analyze","vacuum","freeze"],
		"maintenance_windows":["weekdays 01:00-05:00"],
		"lock_duration_ceiling_ms":3000,
		"blast_radius":{"max_rows_rewritten":5000000,"max_tables_per_window":20},
		"unknown_classification":"fail_closed",
		"budgets":{"storage_bytes":0,"spend_daily":null,"llm_tokens_daily":500000},
		"rate_limits":{"max_self_initiated_changes_per_window":50},
		"deadline_overrides":{"xid":false,"disk":false},
		"refusal_set":["rls_change","grant_expansion"],
		"serialize_mode":"park"
	}`
}
