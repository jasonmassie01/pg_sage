package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

const markActor = "agent:claude-code"

// factRow is a sage.facts row as stored.
type factRow struct {
	Type, Kind, Subject, Status, Source, ProposedBy string
	Value                                           map[string]string
	Decided                                         bool
	Proposals                                       int
}

func readFact(t *testing.T, f *fixture, id int64) factRow {
	t.Helper()
	var r factRow
	var value []byte
	err := f.pool.QueryRow(f.ctx, `SELECT fact_type, subject_kind, subject, status, source,
		proposed_by, value, decided_by IS NOT NULL OR decided_at IS NOT NULL, proposals
		FROM sage.facts WHERE id = $1`, id).Scan(&r.Type, &r.Kind, &r.Subject, &r.Status,
		&r.Source, &r.ProposedBy, &value, &r.Decided, &r.Proposals)
	require.NoError(t, err, "fact %d", id)
	require.NoError(t, json.Unmarshal(value, &r.Value))
	return r
}

// cleanFacts deletes the facts whose subject starts with prefix when the
// test ends.
func cleanFacts(t *testing.T, f *fixture, prefix string) {
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.Background(),
			"DELETE FROM sage.facts WHERE starts_with(subject, $1)", prefix)
		if err != nil {
			t.Errorf("clean facts %s: %v", prefix, err)
		}
	})
}

// markTable creates <schema>.accounts with index <schema>.accounts_email_idx.
func markTable(f *fixture) {
	f.t.Helper()
	f.exec("CREATE TABLE "+f.q("accounts")+" (id int PRIMARY KEY, email text)",
		"CREATE INDEX accounts_email_idx ON "+f.q("accounts")+" (email)")
	cleanFacts(f.t, f, f.schema+".")
}

func ownedRequest(f *fixture) MarkRequest {
	return MarkRequest{Kind: "index", Subject: f.q("accounts_email_idx"), Mark: "owned",
		Repo: "github.com/acme/shop", Path: "db/migrate/20260101_add_email_idx.rb",
		Evidence: "the migration file creates this index"}
}

func TestMarkObjectOwnedProposesFact(t *testing.T) {
	f := newFixture(t)
	markTable(f)
	tools := New(f.pool, Options{})
	res, err := tools.MarkObject(f.ctx, ownedRequest(f), markActor)
	require.NoError(t, err)
	require.True(t, res.Created)
	require.Positive(t, res.FactID)
	require.Equal(t, "proposed", res.Status)
	require.NotEmpty(t, res.Summary)
	row := readFact(t, f, res.FactID)
	require.Equal(t, factRow{Type: "owned_by_app_migrations", Kind: "index",
		Subject: f.q("accounts_email_idx"), Status: "proposed", Source: "operator",
		ProposedBy: markActor, Proposals: 1,
		Value: map[string]string{"repo": "github.com/acme/shop",
			"path": "db/migrate/20260101_add_email_idx.rb"}}, row)
	require.False(t, row.Decided, "MarkObject never decides its own fact")
}

func TestMarkObjectRepeatIsDeduplicated(t *testing.T) {
	f := newFixture(t)
	markTable(f)
	tools := New(f.pool, Options{})
	first, err := tools.MarkObject(f.ctx, ownedRequest(f), markActor)
	require.NoError(t, err)
	second, err := tools.MarkObject(f.ctx, ownedRequest(f), markActor)
	require.NoError(t, err)
	require.False(t, second.Created, "an identical mark is not a new fact")
	require.Equal(t, first.FactID, second.FactID)
	require.Equal(t, "proposed", second.Status)
	require.Equal(t, int64(1), f.count("sage.facts WHERE subject = $1",
		f.q("accounts_email_idx")))
	row := readFact(t, f, first.FactID)
	require.Equal(t, "proposed", row.Status)
	require.Equal(t, 2, row.Proposals)
}

// Concurrent identical marks still make exactly one fact, created once.
func TestMarkObjectConcurrentMarksMakeOneFact(t *testing.T) {
	f := newFixture(t)
	markTable(f)
	tools := New(f.pool, Options{})
	const n = 6
	var wg sync.WaitGroup
	results := make([]MarkResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = tools.MarkObject(f.ctx, ownedRequest(f), markActor)
		}(i)
	}
	wg.Wait()
	created := 0
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, results[0].FactID, results[i].FactID)
		if results[i].Created {
			created++
		}
	}
	require.Equal(t, 1, created, "exactly one call creates the fact")
	require.Equal(t, int64(1), f.count("sage.facts WHERE subject = $1",
		f.q("accounts_email_idx")))
}

func TestMarkObjectExemptNote(t *testing.T) {
	f := newFixture(t)
	markTable(f)
	res, err := New(f.pool, Options{}).MarkObject(f.ctx, MarkRequest{Kind: "table",
		Subject: f.q("accounts"), Mark: "exempt", Evidence: "managed by the billing team"},
		markActor)
	require.NoError(t, err)
	row := readFact(t, f, res.FactID)
	require.Equal(t, "owned_by_app_migrations", row.Type)
	require.Equal(t, "table", row.Kind)
	require.True(t, strings.HasPrefix(row.Value["note"], "exempt"), "note %q", row.Value["note"])
	require.Contains(t, row.Value["note"], "managed by the billing team")
	require.Equal(t, "proposed", row.Status)
}

func TestMarkObjectUnknownObjectNotFound(t *testing.T) {
	f := newFixture(t)
	markTable(f)
	tools := New(f.pool, Options{})
	for _, req := range []MarkRequest{
		{Kind: "table", Subject: f.q("no_such_table"), Mark: "owned", Evidence: "x"},
		{Kind: "index", Subject: f.q("no_such_idx"), Mark: "owned", Evidence: "x"},
	} {
		_, err := tools.MarkObject(f.ctx, req, markActor)
		require.ErrorIs(t, err, ErrNotFound, "subject %s", req.Subject)
	}
	require.Equal(t, int64(0), f.count("sage.facts WHERE starts_with(subject, $1)", f.schema))
}

// Patterns are not checked against the catalog: they name future objects.
func TestMarkObjectPatternsSkipExistenceCheck(t *testing.T) {
	f := newFixture(t)
	prefix := uniqueName("app")
	cleanFacts(t, f, prefix)
	cleanFacts(t, f, f.schema+".")
	tools := New(f.pool, Options{})
	schemaMark, err := tools.MarkObject(f.ctx, MarkRequest{Kind: "schema",
		Subject: prefix + "_*", Mark: "owned", Repo: "github.com/acme/app",
		Evidence: "every app schema is created by migrations"}, markActor)
	require.NoError(t, err)
	require.True(t, schemaMark.Created)
	require.Equal(t, prefix+"_*", readFact(t, f, schemaMark.FactID).Subject)
	tableMark, err := tools.MarkObject(f.ctx, MarkRequest{Kind: "table",
		Subject: f.q("events_*"), Mark: "owned", Evidence: "partitions from migrations"},
		markActor)
	require.NoError(t, err)
	require.Equal(t, "table", readFact(t, f, tableMark.FactID).Kind)
}

func TestMarkObjectProtectedSubject(t *testing.T) {
	f := newFixture(t)
	tools := New(f.pool, Options{})
	for _, req := range []MarkRequest{
		{Kind: "table", Subject: "sage.findings", Mark: "owned", Evidence: "x"},
		{Kind: "schema", Subject: "sag*", Mark: "exempt", Evidence: "x"},
		{Kind: "table", Subject: "pg_catalog.pg_class", Mark: "owned", Evidence: "x"},
	} {
		_, err := tools.MarkObject(f.ctx, req, markActor)
		require.ErrorIs(t, err, facts.ErrProtectedSubject, "subject %s", req.Subject)
	}
	require.Equal(t, int64(0), f.count("sage.facts WHERE subject IN "+
		"('sage.findings', 'sag*', 'pg_catalog.pg_class')"))
}

func TestMarkObjectInvalidRequests(t *testing.T) {
	f := newFixture(t)
	markTable(f)
	tools := New(f.pool, Options{})
	base := ownedRequest(f)
	mutate := map[string]func(*MarkRequest){
		"mark delete":    func(r *MarkRequest) { r.Mark = "delete" },
		"mark empty":     func(r *MarkRequest) { r.Mark = "" },
		"mark confirmed": func(r *MarkRequest) { r.Mark = "confirmed" },
		"empty evidence": func(r *MarkRequest) { r.Evidence = "  " },
		"slot kind":      func(r *MarkRequest) { r.Kind = "slot" },
		"empty subject":  func(r *MarkRequest) { r.Subject = "" },
		"injection subject": func(r *MarkRequest) {
			r.Subject = f.q("accounts_email_idx") + "; DROP TABLE " + f.q("accounts")
		},
	}
	for name, change := range mutate {
		req := base
		change(&req)
		_, err := tools.MarkObject(f.ctx, req, markActor)
		require.Error(t, err, name)
		ok := errors.Is(err, ErrInvalid) || errors.Is(err, facts.ErrInvalidSubject) ||
			errors.Is(err, facts.ErrInvalidKind)
		// to_regclass may read a malformed name as "no such object" first.
		ok = ok || (name == "injection subject" && errors.Is(err, ErrNotFound))
		require.True(t, ok, "%s: undistinguishable error %v", name, err)
	}
	_, err := tools.MarkObject(f.ctx, base, "  ")
	require.ErrorIs(t, err, ErrInvalid, "an actor is required")
	require.True(t, f.relationExists(f.q("accounts")), "injected subject dropped the table")
	require.Equal(t, int64(0), f.count("sage.facts WHERE starts_with(subject, $1)", f.schema))
}

// A fact an operator already decided keeps its decision: a mark only
// re-proposes, it never confirms or reopens.
func TestMarkObjectDoesNotChangeDecidedFact(t *testing.T) {
	f := newFixture(t)
	markTable(f)
	tools := New(f.pool, Options{})
	first, err := tools.MarkObject(f.ctx, ownedRequest(f), markActor)
	require.NoError(t, err)
	f.execArgs(`UPDATE sage.facts SET status = 'rejected', decided_by = 'alice',
		decided_at = now() WHERE id = $1`, first.FactID)
	again, err := tools.MarkObject(f.ctx, ownedRequest(f), markActor)
	require.NoError(t, err)
	require.False(t, again.Created)
	require.Equal(t, "rejected", again.Status)
	require.Equal(t, "rejected", readFact(t, f, first.FactID).Status)
}
