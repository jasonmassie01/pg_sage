package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// fleetpol-01a on a real server: the store resolves every database through
// its policy set, the gate reads the resolved document, exceptions hold
// and expire, and a v2.5.0 install keeps its per-database documents.

const prodOnlyAnalyze = `{
	"allowed_change_classes":["analyze"],
	"maintenance_windows":["always"],
	"lock_duration_ceiling_ms":3000,
	"blast_radius":{"max_rows_rewritten":5000000,"max_tables_per_window":20},
	"unknown_classification":"fail_closed",
	"budgets":{"storage_bytes":0,"spend_daily":null,"llm_tokens_daily":500000},
	"rate_limits":{"max_self_initiated_changes_per_window":50},
	"deadline_overrides":{"xid":false,"disk":false},
	"refusal_set":["rls_change","grant_expansion"],
	"serialize_mode":"park"
}`

func newSetsStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	store := newTestStore(t)
	pool := store.pool
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
TRUNCATE sage.policy_exceptions, sage.policy_set_events;
DELETE FROM sage.policy_sets;
TRUNCATE sage.databases RESTART IDENTITY CASCADE;
INSERT INTO sage.policy_sets (name, priority, selectors, created_by) VALUES
  ('default', 0, '[{"all": true}]', 'test'),
  ('prod', 10, '[{"tags": {"tier": "prod"}}]', 'test')`)
	require.NoError(t, err)
	return store, pool
}

func addDatabase(t *testing.T, pool *pgxpool.Pool, name string, tags map[string]string) int64 {
	t.Helper()
	raw, err := json.Marshal(tags)
	require.NoError(t, err)
	var id int64
	err = pool.QueryRow(context.Background(), `
INSERT INTO sage.databases (name, host, database_name, username, password_enc, tags)
VALUES ($1, 'localhost', $1, 'sage', '\x00', $2) RETURNING id`, name, raw).Scan(&id)
	require.NoError(t, err)
	return id
}

// bootstrapSets gives every set its first document, as a sidecar start
// does, and returns them by name.
func bootstrapSets(t *testing.T, store *Store, databaseID int64) map[string]Policy {
	t.Helper()
	_, err := store.Bootstrap(context.Background(), Scope{DatabaseID: &databaseID},
		"unattended", "bootstrap")
	require.NoError(t, err)
	sets, err := store.Sets(context.Background())
	require.NoError(t, err)
	out := map[string]Policy{}
	for _, s := range sets {
		if s.Policy == nil {
			t.Fatalf("set %s has no document after bootstrap", s.Set.Name)
		}
		out[s.Set.Name] = *s.Policy
	}
	return out
}

// narrowProd ratifies prodOnlyAnalyze as the prod set's version 2 through
// propose-then-ratify, the only way a set's document changes.
func narrowProd(t *testing.T, store *Store) Policy {
	t.Helper()
	ctx := context.Background()
	scope := Scope{SetName: "prod"}
	proposal, err := store.Propose(ctx, ProposalRequest{Scope: scope, ExpectedVersion: 1,
		Profile: "unattended", Document: json.RawMessage(prodOnlyAnalyze), Actor: "dba"})
	require.NoError(t, err)
	ratified, err := store.Ratify(ctx, RatifyRequest{ProposalID: proposal.ID,
		ExpectedVersion: 1, Actor: "admin"})
	require.NoError(t, err)
	require.Equal(t, int64(2), ratified.Version)
	require.Equal(t, "prod", ratified.Scope.SetName)
	return ratified
}

func TestPolicySetDB_ProdTaggedDatabaseGateReadsProd(t *testing.T) {
	store, pool := newSetsStore(t)
	ctx := context.Background()
	prodID := addDatabase(t, pool, "orders", map[string]string{"tier": "prod"})
	devID := addDatabase(t, pool, "scratch", map[string]string{"tier": "dev"})
	docs := bootstrapSets(t, store, prodID)
	prodV2 := narrowProd(t, store)

	current, err := store.Current(ctx, Scope{DatabaseID: &prodID})
	require.NoError(t, err)
	require.Equal(t, prodV2.ID, current.ID)
	require.Equal(t, "prod", current.Set)
	require.Equal(t, prodID, *current.Scope.DatabaseID)
	doc, err := DocumentFromPolicy(current)
	require.NoError(t, err)
	require.Equal(t, []ChangeClass{ChangeAnalyze}, doc.AllowedChangeClasses)
	sum := sha256.Sum256(prodV2.Document)
	require.Equal(t, hex.EncodeToString(sum[:]), doc.SHA256)
	require.Equal(t, int64(2), doc.Version)

	gate := newTestGate(t, gateFixture{policy: doc})
	decision := gate.Authorize(ctx, validIndexRequest())
	assertDecision(t, decision, VerdictBlocked, ReasonChangeClassNotAllowed)
	require.Equal(t, int64(2), decision.PolicyVersion)

	dev, err := store.Current(ctx, Scope{DatabaseID: &devID})
	require.NoError(t, err)
	require.Equal(t, docs[DefaultSetName].ID, dev.ID)
	members, err := store.Members(ctx, "prod")
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, prodID, members[0].Member.DatabaseID)
	if _, err := store.Members(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("members of an unknown set = %v, want ErrNotFound", err)
	}
}

func TestPolicySetDB_ControlReadFailureIsPolicyUnavailable(t *testing.T) {
	store, pool := newSetsStore(t)
	id := addDatabase(t, pool, "orders", map[string]string{"tier": "prod"})
	bootstrapSets(t, store, id)
	closed, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	require.NoError(t, err)
	closed.Close()
	_, readErr := NewStore(closed).Current(context.Background(), Scope{DatabaseID: &id})
	if readErr == nil || errors.Is(readErr, ErrNotFound) {
		t.Fatalf("a failed control read returned %v, want a read error", readErr)
	}
	gate := newTestGate(t, gateFixture{policyErr: readErr})
	decision := gate.Authorize(context.Background(), validIndexRequest())
	assertDecision(t, decision, VerdictBlocked, ReasonPolicyUnavailable)
}

func TestPolicySetDB_PutRefusesTiesUnknownKeysAndDimensions(t *testing.T) {
	store, pool := newSetsStore(t)
	ctx := context.Background()
	id := addDatabase(t, pool, "orders", map[string]string{"tier": "prod", "team": "a"})
	bootstrapSets(t, store, id)
	twin := testSet("team-a", 10, Selector{Tags: map[string]string{"team": "a"}})
	_, err := store.PutSet(ctx, PutSetRequest{Set: twin, Actor: "admin",
		Dimensions: testDimensions})
	if !errors.Is(err, ErrSetConflict) {
		t.Fatalf("a tie with prod on database %d was saved: %v", id, err)
	}
	bad := withGrades(testSet("team-a", 11, Selector{All: true}),
		GradeRequirement{Dimension: "vibes", MinLetter: "C", WithinHours: 1})
	if _, err := store.PutSet(ctx, PutSetRequest{Set: bad, Actor: "admin",
		Dimensions: testDimensions}); !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("an unknown dimension was saved: %v", err)
	}
	if _, err := store.PutSet(ctx, PutSetRequest{Set: prodSet()}); !errors.Is(err,
		ErrInvalidSet) {
		t.Fatalf("a PUT without an actor was saved: %v", err)
	}
	twin.Priority = 15
	saved, err := store.PutSet(ctx, PutSetRequest{Set: twin, Actor: "admin",
		Dimensions: testDimensions})
	require.NoError(t, err)
	require.NotNil(t, saved.Policy)
	require.Equal(t, int64(1), saved.Policy.Version)
	sets, err := store.Sets(ctx)
	require.NoError(t, err)
	var defaultDoc json.RawMessage
	for _, s := range sets {
		if s.Set.Name == DefaultSetName {
			defaultDoc = s.Policy.Document
		}
	}
	require.JSONEq(t, string(defaultDoc), string(saved.Policy.Document))
	current, err := store.Current(ctx, Scope{DatabaseID: &id})
	require.NoError(t, err)
	require.Equal(t, "team-a", current.Set)
	var events int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM sage.policy_set_events
		WHERE event = 'set_saved' AND set_name = 'team-a' AND actor = 'admin'`).Scan(&events))
	require.Equal(t, 1, events)
}

func TestPolicySetDB_ExceptionHoldsThenExpiresWithAudit(t *testing.T) {
	store, pool := newSetsStore(t)
	ctx := context.Background()
	id := addDatabase(t, pool, "orders", map[string]string{"tier": "prod"})
	docs := bootstrapSets(t, store, id)
	now := time.Now().UTC().Truncate(time.Second)
	expires := now.Add(time.Hour)
	exc, err := store.SetException(ctx, ExceptionRequest{DatabaseID: id, Actor: "dba",
		Reason: "nightly batch needs vacuum", ExpiresAt: &expires})
	require.NoError(t, err)
	require.Equal(t, "prod", exc.SetName)
	own, err := store.Current(ctx, Scope{DatabaseID: &id})
	require.NoError(t, err)
	require.NotEqual(t, docs["prod"].ID, own.ID)
	require.Equal(t, "", own.Set)
	require.JSONEq(t, string(docs["prod"].Document), string(own.Document))
	assertExceptionRefusals(t, store, id, now)

	held := store.WithClock(func() time.Time { return expires.Add(-time.Second) })
	current, err := held.Current(ctx, Scope{DatabaseID: &id})
	require.NoError(t, err)
	require.Equal(t, own.ID, current.ID)
	lapsed := store.WithClock(func() time.Time { return expires })
	current, err = lapsed.Current(ctx, Scope{DatabaseID: &id})
	require.NoError(t, err)
	require.Equal(t, docs["prod"].ID, current.ID)

	n, err := held.ExpireExceptions(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	n, err = lapsed.ExpireExceptions(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	list, err := store.Exceptions(ctx)
	require.NoError(t, err)
	require.Len(t, list, 0)
	var audit int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM sage.policy_set_events
		WHERE event = 'exception_expired' AND database_id = $1`, id).Scan(&audit))
	require.Equal(t, 1, audit)
	current, err = store.Current(ctx, Scope{DatabaseID: &id})
	require.NoError(t, err)
	require.Equal(t, docs["prod"].ID, current.ID)
}

// A proposal on a database that follows a set builds on the set's
// document; ratified, it is that database's own document (an exception),
// and the set and its other members keep the set's document.
func TestPolicySetDB_RatifiedDatabaseProposalBecomesAnException(t *testing.T) {
	store, pool := newSetsStore(t)
	ctx := context.Background()
	id := addDatabase(t, pool, "orders", map[string]string{"tier": "prod"})
	other := addDatabase(t, pool, "billing", map[string]string{"tier": "prod"})
	docs := bootstrapSets(t, store, id)
	base, err := store.Current(ctx, Scope{DatabaseID: &id})
	require.NoError(t, err)
	require.Equal(t, docs["prod"].ID, base.ID)
	proposal, err := store.Propose(ctx, ProposalRequest{Scope: base.Scope,
		ExpectedVersion: base.Version, Profile: "staffed",
		Document: json.RawMessage(staffedDocument()), Actor: "dba"})
	require.NoError(t, err)
	require.Equal(t, id, *proposal.Scope.DatabaseID)
	ratified, err := store.Ratify(ctx, RatifyRequest{ProposalID: proposal.ID,
		ExpectedVersion: base.Version, Actor: "admin"})
	require.NoError(t, err)
	require.Equal(t, base.Version+1, ratified.Version)
	require.Equal(t, "", ratified.Set)

	current, err := store.Current(ctx, Scope{DatabaseID: &id})
	require.NoError(t, err)
	require.Equal(t, ratified.ID, current.ID)
	otherCurrent, err := store.Current(ctx, Scope{DatabaseID: &other})
	require.NoError(t, err)
	require.Equal(t, docs["prod"].ID, otherCurrent.ID)
	setRow, err := store.Current(ctx, Scope{SetName: "prod"})
	require.NoError(t, err)
	require.Equal(t, docs["prod"].ID, setRow.ID)
	list, err := store.Exceptions(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, id, list[0].DatabaseID)
	require.Equal(t, "prod", list[0].SetName)
	require.Equal(t, "admin", list[0].SetBy)
	if len(list[0].Diff) == 0 {
		t.Fatal("the exception differs from prod but lists no diff")
	}
	assertStaleRatifyConflicts(t, store, id, ratified.Version, base.Version)
}

// assertStaleRatifyConflicts: a proposal ratified against the set's old
// version once the database has its own document is a version conflict.
func assertStaleRatifyConflicts(t *testing.T, store *Store, id, current, stale int64) {
	t.Helper()
	ctx := context.Background()
	proposal, err := store.Propose(ctx, ProposalRequest{Scope: Scope{DatabaseID: &id},
		ExpectedVersion: current, Profile: "staffed",
		Document: json.RawMessage(staffedDocument()), Actor: "dba"})
	require.NoError(t, err)
	_, err = store.Ratify(ctx, RatifyRequest{ProposalID: proposal.ID,
		ExpectedVersion: stale, Actor: "admin"})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("ratifying against a stale version = %v, want ErrVersionConflict", err)
	}
}

// assertExceptionRefusals: a second exception, one expiring in the past
// and one without a reason are refused.
func assertExceptionRefusals(t *testing.T, store *Store, id int64, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.SetException(ctx, ExceptionRequest{DatabaseID: id, Actor: "dba",
		Reason: "again"}); !errors.Is(err, ErrExceptionExists) {
		t.Fatalf("a second exception = %v, want ErrExceptionExists", err)
	}
	past := now.Add(-time.Minute)
	if _, err := store.SetException(ctx, ExceptionRequest{DatabaseID: id + 1, Actor: "dba",
		Reason: "r", ExpiresAt: &past}); !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("an exception expiring in the past = %v, want ErrInvalidSet", err)
	}
	if _, err := store.SetException(ctx, ExceptionRequest{DatabaseID: id + 1,
		Actor: "dba"}); !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("an exception without a reason = %v, want ErrInvalidSet", err)
	}
}

// A v2.5.0 install: two per-database documents, no sets. After the sets
// arrive and bootstrap runs, each database keeps its document (same hash)
// and is listed as an exception against default.
func TestPolicySetDB_UpgradeKeepsPerDatabaseDocumentsAsExceptions(t *testing.T) {
	store, pool := newSetsStore(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `DELETE FROM sage.policy_sets`)
	require.NoError(t, err)
	before := map[int64]string{}
	for i, doc := range []string{staffedDocument(), prodOnlyAnalyze} {
		id := addDatabase(t, pool, fmt.Sprintf("legacy%d", i), nil)
		_, err := pool.Exec(ctx, `INSERT INTO sage.policy (database_id, version, scope,
			profile, doc, status, proposed_by, ratified_by, ratified_at, activated_at)
			VALUES ($1, 1, 'database', 'staffed', $2, 'active', 'v250', 'v250', now(), now())`,
			id, doc)
		require.NoError(t, err)
		current, err := store.Current(ctx, Scope{DatabaseID: &id})
		require.NoError(t, err)
		d, err := DocumentFromPolicy(current)
		require.NoError(t, err)
		before[id] = d.SHA256
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.policy_sets (name, priority, selectors,
		created_by) VALUES ('default', 0, '[{"all": true}]', 'migration')`)
	require.NoError(t, err)
	for id, sha := range before {
		_, err := store.Bootstrap(ctx, Scope{DatabaseID: &id}, "unattended", "bootstrap")
		require.NoError(t, err)
		current, err := store.Current(ctx, Scope{DatabaseID: &id})
		require.NoError(t, err)
		d, err := DocumentFromPolicy(current)
		require.NoError(t, err)
		require.Equal(t, sha, d.SHA256)
	}
	list, err := store.Exceptions(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, e := range list {
		require.Equal(t, DefaultSetName, e.SetName)
		require.Equal(t, "v250", e.SetBy)
		if _, ok := before[e.DatabaseID]; !ok {
			t.Fatalf("exception for unknown database %d", e.DatabaseID)
		}
		if len(e.Diff) == 0 {
			t.Fatalf("database %d differs from default but lists no diff", e.DatabaseID)
		}
	}
}

// Concurrent access: a PUT that moves the database from default to prod
// lands while 100 gate reads run. Every read names one of the two set
// documents, and its hash is that document's hash.
func TestPolicySetDB_PutDuringGateReadsNeverMixes(t *testing.T) {
	store, pool := newSetsStore(t)
	ctx := context.Background()
	id := addDatabase(t, pool, "orders", map[string]string{"tier": "staging"})
	docs := bootstrapSets(t, store, id)
	prodV2 := narrowProd(t, store)
	want := map[int64]string{}
	for _, p := range []Policy{docs[DefaultSetName], prodV2} {
		sum := sha256.Sum256(p.Document)
		want[p.ID] = hex.EncodeToString(sum[:])
	}
	moved := prodSet()
	moved.Selectors = []Selector{{Tags: map[string]string{"tier": "staging"}}}
	var wg sync.WaitGroup
	errs := make(chan error, 101)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			current, err := store.Current(ctx, Scope{DatabaseID: &id})
			if err != nil {
				errs <- err
				return
			}
			doc, err := DocumentFromPolicy(current)
			if err != nil || want[current.ID] == "" || doc.SHA256 != want[current.ID] {
				errs <- fmt.Errorf("read id %d sha %s err %v", current.ID, doc.SHA256, err)
			}
		}()
	}
	if _, err := store.PutSet(ctx, PutSetRequest{Set: moved, Actor: "admin",
		Dimensions: testDimensions}); err != nil {
		errs <- err
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	after, err := store.Current(ctx, Scope{DatabaseID: &id})
	require.NoError(t, err)
	require.Equal(t, prodV2.ID, after.ID)
}
