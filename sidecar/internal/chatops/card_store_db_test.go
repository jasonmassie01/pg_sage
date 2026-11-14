package chatops

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// A card token is minted per card per channel, stored only as its
// SHA-256, valid until it expires, and consumed at most once.

func cardIssue(queueID int) CardIssue {
	return CardIssue{ChannelID: 7, Database: "orders", QueueID: queueID,
		CardHash: "h-" + time.Now().Format(time.RFC3339Nano), Title: "Create index",
		Summary: "38% faster on 3 queries", ExpiresAt: time.Now().Add(time.Hour)}
}

func liveCards(t *testing.T) (*CardStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	_, pool, ctx := liveStore(t)
	return NewCardStore(pool), pool, ctx
}

func TestCardIssueLookupStoresOnlyTheHash(t *testing.T) {
	s, pool, ctx := liveCards(t)
	in := cardIssue(101)
	token, err := s.Issue(ctx, in)
	if err != nil || !ValidCardToken(token) {
		t.Fatalf("issue = %q, %v", token, err)
	}
	other, err := s.Issue(ctx, in)
	if err != nil || other == token {
		t.Fatalf("second token %q (first %q), %v", other, token, err)
	}
	var raw int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.approval_card_deliveries d
		WHERE row_to_json(d)::text LIKE '%' || $1 || '%'`, token).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	var hashed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.approval_card_deliveries
		WHERE token_sha256 = $1`, sum[:]).Scan(&hashed); err != nil {
		t.Fatal(err)
	}
	if raw != 0 || hashed != 1 {
		t.Fatalf("raw matches %d, hashed matches %d", raw, hashed)
	}
	got, err := s.Lookup(ctx, token)
	if err != nil || got.ChannelID != 7 || got.Database != "orders" || got.QueueID != 101 ||
		got.CardHash != in.CardHash || got.Title != "Create index" ||
		got.Summary != in.Summary || got.UsedAt != nil || got.ID <= 0 ||
		!got.ExpiresAt.After(time.Now()) {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
}

func TestCardIssueValidates(t *testing.T) {
	s, _, ctx := liveCards(t)
	for name, mutate := range map[string]func(*CardIssue){
		"no channel":   func(c *CardIssue) { c.ChannelID = 0 },
		"no database":  func(c *CardIssue) { c.Database = "" },
		"no queue":     func(c *CardIssue) { c.QueueID = 0 },
		"no hash":      func(c *CardIssue) { c.CardHash = "" },
		"already over": func(c *CardIssue) { c.ExpiresAt = time.Now().Add(-time.Second) },
	} {
		in := cardIssue(1)
		mutate(&in)
		if _, err := s.Issue(ctx, in); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
		}
	}
}

func TestCardLookupUnknownAndMalformed(t *testing.T) {
	s, _, ctx := liveCards(t)
	if _, err := s.Lookup(ctx, "ZZZZZZZZZZZZZZZZZZZZZZ"); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("forged token: err = %v, want ErrCardUnknown", err)
	}
	if _, err := s.Lookup(ctx, "short"); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("malformed token: err = %v, want ErrCardUnknown", err)
	}
	if _, err := s.Consume(ctx, "ZZZZZZZZZZZZZZZZZZZZZZ", 1, DecisionApprove,
		0); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("consume forged: err = %v", err)
	}
}

func TestCardConsumeOnceThenReplay(t *testing.T) {
	s, _, ctx := liveCards(t)
	token, err := s.Issue(ctx, cardIssue(202))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Consume(ctx, token, 5, DecisionApprove, 9001)
	if err != nil || got.UsedAt == nil || got.UsedBy == nil || *got.UsedBy != 5 ||
		got.Decision != string(DecisionApprove) || got.MessageID != 9001 || got.QueueID != 202 {
		t.Fatalf("consume = %+v, %v", got, err)
	}
	if _, err := s.Consume(ctx, token, 6, DecisionDeny, 0); !errors.Is(err, ErrCardUsed) {
		t.Fatalf("replay: err = %v, want ErrCardUsed", err)
	}
	again, err := s.Lookup(ctx, token)
	if err != nil || again.Decision != string(DecisionApprove) || *again.UsedBy != 5 {
		t.Fatalf("a replay changed the record: %+v, %v", again, err)
	}
}

func TestCardConsumeExpired(t *testing.T) {
	s, pool, ctx := liveCards(t)
	token, err := s.Issue(ctx, cardIssue(303))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(token))
	if _, err := pool.Exec(ctx, `UPDATE sage.approval_card_deliveries
		SET expires_at = now() - interval '1 second' WHERE token_sha256 = $1`,
		sum[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(ctx, token, 5, DecisionApprove, 0); !errors.Is(err, ErrCardExpired) {
		t.Fatalf("expired: err = %v, want ErrCardExpired", err)
	}
	got, err := s.Lookup(ctx, token)
	// expires_at is the database's now() - 1 s: judge it on that clock (ci-14).
	if err != nil || got.UsedAt != nil || !got.Expired(testdb.MustDBNow(t, pool)) {
		t.Fatalf("expired card = %+v, %v", got, err)
	}
}

func TestCardRevoke(t *testing.T) {
	s, _, ctx := liveCards(t)
	token, err := s.Issue(ctx, cardIssue(404))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, token); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.Lookup(ctx, token); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("revoked card: err = %v", err)
	}
	if err := s.Revoke(ctx, token); err != nil {
		t.Fatalf("revoking twice must be harmless: %v", err)
	}
}

// Concurrent presses of one card (two approvers, a double click):
// exactly one consumes it.
func TestCardConsumeConcurrentExactlyOnce(t *testing.T) {
	s, _, ctx := liveCards(t)
	token, err := s.Issue(ctx, cardIssue(505))
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, used := 0, 0
	for i := range n {
		wg.Add(1)
		go func(user int) {
			defer wg.Done()
			_, err := s.Consume(ctx, token, user, DecisionApprove, 0)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrCardUsed):
				used++
			default:
				t.Errorf("consume: %v", err)
			}
		}(i + 1)
	}
	wg.Wait()
	if ok != 1 || used != n-1 {
		t.Fatalf("successes %d, replays %d", ok, used)
	}
}

func TestCardFollowupQueue(t *testing.T) {
	s, pool, ctx := liveCards(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.approval_card_deliveries`); err != nil {
		t.Fatal(err)
	}
	a, err := s.Issue(ctx, cardIssue(606))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Issue(ctx, cardIssue(607)); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingFollowups(ctx, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	first, _ := s.Lookup(ctx, a)
	if err := s.MarkFollowedUp(ctx, first.ID, "verified"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	pending, err = s.PendingFollowups(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].QueueID != 607 {
		t.Fatalf("pending after mark = %+v, %v", pending, err)
	}
	if err := s.MarkFollowedUp(ctx, first.ID, "verified"); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("marking twice: err = %v, want ErrCardUnknown", err)
	}
	limited, err := s.PendingFollowups(ctx, 0)
	if err != nil || len(limited) != 0 {
		t.Fatalf("limit 0 = %+v, %v", limited, err)
	}
	got, _ := s.Lookup(ctx, a)
	if got.FollowupVerdict != "verified" || got.FollowedUpAt == nil {
		t.Fatalf("followed-up card = %+v", got)
	}
}

// policy-06: a new card for the same item in the same chat closes the
// older open ones, so the verdict is posted once per chat.
func TestCardIssueSupersedesOlderOpenCardsOfTheSameChannel(t *testing.T) {
	s, _, ctx := liveCards(t)
	base := cardIssue(808)
	a1 := issueOK(t, ctx, s, base)
	otherChannel := base
	otherChannel.ChannelID = 8
	b1 := issueOK(t, ctx, s, otherChannel)
	otherDB := base
	otherDB.Database = "billing"
	c1 := issueOK(t, ctx, s, otherDB)
	if _, err := s.Consume(ctx, a1, 1, DecisionSnooze, 55); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	a2 := issueOK(t, ctx, s, base)
	got := lookupOK(t, ctx, s, a1)
	if got.FollowedUpAt == nil || got.FollowupVerdict != "superseded" ||
		got.Decision != "snooze" || got.MessageID != 55 {
		t.Fatalf("older snoozed card = %+v, want superseded with its decision kept", got)
	}
	for name, tok := range map[string]string{"other channel": b1, "other database": c1,
		"new card": a2} {
		if d := lookupOK(t, ctx, s, tok); d.FollowedUpAt != nil || d.FollowupVerdict != "" {
			t.Fatalf("%s card was closed: %+v", name, d)
		}
	}
	// A failed insert closes nothing: the statement is one unit.
	bad := base
	bad.CardHash = strings.Repeat("x", 200)
	if _, err := s.Issue(ctx, bad); err == nil {
		t.Fatal("an over-long card hash was accepted")
	}
	if d := lookupOK(t, ctx, s, a2); d.FollowedUpAt != nil {
		t.Fatalf("a failed insert superseded the open card: %+v", d)
	}
	// A card already followed up keeps its verdict.
	b := lookupOK(t, ctx, s, b1)
	if err := s.MarkFollowedUp(ctx, b.ID, "verified"); err != nil {
		t.Fatal(err)
	}
	issueOK(t, ctx, s, otherChannel)
	if d := lookupOK(t, ctx, s, b1); d.FollowupVerdict != "verified" {
		t.Fatalf("followed-up card rewritten to %q", d.FollowupVerdict)
	}
}

func issueOK(t *testing.T, ctx context.Context, s *CardStore, in CardIssue) string {
	t.Helper()
	token, err := s.Issue(ctx, in)
	if err != nil {
		t.Fatalf("issue %+v: %v", in, err)
	}
	return token
}

func lookupOK(t *testing.T, ctx context.Context, s *CardStore, token string) CardDelivery {
	t.Helper()
	d, err := s.Lookup(ctx, token)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	return d
}

func TestCardFollowupsWaitForTheirNextCheck(t *testing.T) {
	s, pool, ctx := liveCards(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.approval_card_deliveries`); err != nil {
		t.Fatal(err)
	}
	a := lookupOK(t, ctx, s, issueOK(t, ctx, s, cardIssue(909)))
	b := lookupOK(t, ctx, s, issueOK(t, ctx, s, cardIssue(910)))
	if a.NextCheckAt.IsZero() || a.NextCheckAt.After(time.Now().Add(5*time.Second)) {
		t.Fatalf("a new card is due at %v, want now", a.NextCheckAt)
	}
	if err := s.DeferFollowup(ctx, a.ID, 10*time.Minute); err != nil {
		t.Fatalf("defer: %v", err)
	}
	pending, err := s.PendingFollowups(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != b.ID {
		t.Fatalf("pending with a deferred = %+v, %v", pending, err)
	}
	a = lookupOK(t, ctx, s, issueOK(t, ctx, s, cardIssue(911)))
	if _, err := pool.Exec(ctx, `UPDATE sage.approval_card_deliveries SET next_check_at =
		CASE id WHEN $1 THEN now() - interval '1 hour' ELSE now() - interval '2 hours' END
		WHERE id IN ($1, $2)`, a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingFollowups(ctx, 10)
	if err != nil || len(pending) != 2 || pending[0].ID != b.ID || pending[1].ID != a.ID {
		t.Fatalf("pending = %+v, %v; want the earliest re-check first", pending, err)
	}
	if err := s.DeferFollowup(ctx, 987654321, time.Minute); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("unknown card: %v", err)
	}
	for _, d := range []time.Duration{0, -time.Second} {
		if err := s.DeferFollowup(ctx, a.ID, d); !errors.Is(err, ErrMalformed) {
			t.Fatalf("delay %v: err = %v, want ErrMalformed", d, err)
		}
	}
	if err := s.MarkFollowedUp(ctx, b.ID, "verified"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferFollowup(ctx, b.ID, time.Minute); !errors.Is(err, ErrCardUnknown) {
		t.Fatalf("deferring a followed-up card: %v", err)
	}
}

func TestCardFollowupQueueSchemaAndPlan(t *testing.T) {
	_, pool, ctx := liveCards(t)
	var nullable, typ, def string
	if err := pool.QueryRow(ctx, `SELECT is_nullable, data_type, column_default
		FROM information_schema.columns WHERE table_schema = 'sage'
		AND table_name = 'approval_card_deliveries' AND column_name = 'next_check_at'`).Scan(
		&nullable, &typ, &def); err != nil {
		t.Fatalf("next_check_at: %v", err)
	}
	if nullable != "NO" || typ != "timestamp with time zone" || !strings.Contains(def, "now()") {
		t.Fatalf("next_check_at is %s %s default %s", nullable, typ, def)
	}
	var idx string
	if err := pool.QueryRow(ctx, `SELECT pg_get_indexdef(
		'sage.idx_approval_card_next_check'::regclass)`).Scan(&idx); err != nil {
		t.Fatalf("index: %v", err)
	}
	if !strings.Contains(idx, "(next_check_at, id) WHERE (followed_up_at IS NULL)") {
		t.Fatalf("index = %s", idx)
	}
	var gone bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sage.idx_approval_card_followup')
		IS NULL`).Scan(&gone); err != nil || !gone {
		t.Fatalf("the replaced index is still there (%v)", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, `EXPLAIN `+pendingFollowupsSQL, 500)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(strings.Join(plan, "\n"), "idx_approval_card_next_check") {
		t.Fatalf("PendingFollowups does not use the partial index:\n%s",
			strings.Join(plan, "\n"))
	}
}

// policy-06 review: an older card pressed after a re-send takes the open
// follow-up back, so the verdict still replies to the decision's message.
// A card whose chat already got its verdict hands nothing back.
func TestCardConsumeOfSupersededCardCarriesTheFollowup(t *testing.T) {
	s, _, ctx := liveCards(t)
	old := issueOK(t, ctx, s, cardIssue(812))
	resent := issueOK(t, ctx, s, cardIssue(812))
	got, err := s.Consume(ctx, old, 1, DecisionApprove, 4242)
	if err != nil {
		t.Fatalf("approve from the older card: %v", err)
	}
	if got.FollowedUpAt != nil || got.FollowupVerdict != "" || got.MessageID != 4242 {
		t.Fatalf("pressed card = %+v, want it open with message 4242", got)
	}
	if d := lookupOK(t, ctx, s, resent); d.FollowedUpAt == nil ||
		d.FollowupVerdict != "superseded" {
		t.Fatalf("newer unused card = %+v, want superseded", d)
	}
	open := lookupOK(t, ctx, s, old)
	if err := s.MarkFollowedUp(ctx, open.ID, "verified"); err != nil {
		t.Fatal(err)
	}
	// The verdict is out: a later press of the superseded card reopens nothing.
	stale := lookupOK(t, ctx, s, resent)
	got, err = s.Consume(ctx, resent, 1, DecisionApprove, 4343)
	if err != nil {
		t.Fatalf("late press: %v", err)
	}
	if got.FollowedUpAt == nil || got.FollowupVerdict != "superseded" {
		t.Fatalf("late press reopened its card: %+v (was %+v)", got, stale)
	}
	if d := lookupOK(t, ctx, s, old); d.FollowupVerdict != "verified" {
		t.Fatalf("posted card rewritten to %q", d.FollowupVerdict)
	}
	// Another chat's open card is not taken over.
	other := cardIssue(813)
	other.ChannelID = 9
	keep := issueOK(t, ctx, s, other)
	mine := issueOK(t, ctx, s, cardIssue(813))
	if _, err := s.Consume(ctx, mine, 1, DecisionDeny, 1); err != nil {
		t.Fatal(err)
	}
	if d := lookupOK(t, ctx, s, keep); d.FollowedUpAt != nil {
		t.Fatalf("another channel's card was closed: %+v", d)
	}
}
