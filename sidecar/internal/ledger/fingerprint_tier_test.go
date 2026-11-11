package ledger

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// audit-11: a fingerprint repeat rewrote risk_tier on the open row, so a
// candidate re-tiered from moderate to high silently changed the authority
// of a decision already recorded (and sealed in the audit chain). A repeat
// at another tier now closes the open row and starts a new decision; the
// fingerprint itself is unchanged, so open rows written before keep
// matching.

// assignsTier matches an assignment of risk_tier in the upsert's SET list
// (the WHERE guard's "sage.decision.risk_tier = ..." is not one).
var assignsTier = regexp.MustCompile(`(^|[\s,])risk_tier\s*=\s*EXCLUDED`)

func TestUpsertNeverRewritesTheTier(t *testing.T) {
	if assignsTier.MatchString(upsertDecisionSQL) ||
		!strings.Contains(upsertDecisionSQL, "WHERE sage.decision.risk_tier = "+
			"EXCLUDED.risk_tier") {
		t.Fatalf("the repeat upsert may rewrite an open decision's tier:\n%s",
			upsertDecisionSQL)
	}
}

type tierRow struct {
	id       int64
	tier     string
	repeats  int
	resolved bool
}

func tierRows(t *testing.T, pool *pgxpool.Pool, fingerprint string) []tierRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id, risk_tier, repeat_count,
		resolved_at IS NOT NULL FROM sage.decision WHERE fingerprint = $1 ORDER BY id`,
		fingerprint)
	if err != nil {
		t.Fatalf("read rows of %s: %v", fingerprint, err)
	}
	defer rows.Close()
	var out []tierRow
	for rows.Next() {
		var r tierRow
		if err := rows.Scan(&r.id, &r.tier, &r.repeats, &r.resolved); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rows of %s: %v", fingerprint, err)
	}
	return out
}

// moderate, moderate, high, high, moderate: three decisions, each at the
// tier it was recorded at; only the last is open.
func TestRetieredRepeatStartsANewDecision(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	input := parkedInput(fmt.Sprintf("fpt_%d", time.Now().UnixNano()), ledgerDatabaseID())
	input.RiskTier = "moderate"
	before := input.Fingerprint
	cleanupFingerprint(t, pool, input.Fingerprint)
	ids := []int64{}
	for _, tier := range []string{"moderate", "moderate", "high", "high", "moderate"} {
		input.RiskTier = tier
		if DecisionFingerprint(input) != before {
			t.Fatal("the tier must not change the fingerprint (open rows keep matching)")
		}
		d, err := service.RecordDecision(ctx, input)
		if err != nil {
			t.Fatalf("RecordDecision at %s: %v", tier, err)
		}
		ids = append(ids, d.ID)
	}
	if ids[0] != ids[1] || ids[2] != ids[3] || ids[1] == ids[2] || ids[3] == ids[4] ||
		ids[4] == ids[0] {
		t.Fatalf("decision ids %v, want rows a a b b c", ids)
	}
	got := tierRows(t, pool, input.Fingerprint)
	want := []tierRow{{ids[0], "moderate", 2, true}, {ids[2], "high", 2, true},
		{ids[4], "moderate", 1, false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
}

// Writers repeating one fingerprint at two tiers at once never leave two
// open rows and never rewrite a row's tier.
func TestConcurrentRetieringKeepsOneOpenRow(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	input := parkedInput(fmt.Sprintf("fptc_%d", time.Now().UnixNano()), ledgerDatabaseID())
	cleanupFingerprint(t, pool, input.Fingerprint)
	var wg sync.WaitGroup
	tiers := map[int64]string{}
	var unexpected []error
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := input
			in.RiskTier = []string{"moderate", "high"}[i%2]
			in.EvidenceID = ledgerEvidenceID(fmt.Sprintf("tc%d", i))
			id, _, err := repo.UpsertDecision(ctx, in)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrDecisionRetiered): // refused, never merged
			case err != nil:
				unexpected = append(unexpected, err)
			default:
				tiers[id] = in.RiskTier
			}
		}(i)
	}
	wg.Wait()
	if len(unexpected) > 0 {
		t.Fatalf("concurrent re-tiering failed other than by refusal: %v", unexpected)
	}
	open := 0
	for _, r := range tierRows(t, pool, input.Fingerprint) {
		if !r.resolved {
			open++
		}
		if want, ok := tiers[r.id]; ok && want != r.tier {
			t.Fatalf("row %d is at %s, but a writer recorded it at %s", r.id, r.tier, want)
		}
	}
	if open != 1 || len(tiers) == 0 {
		t.Fatalf("%d open rows (%d recorded), want exactly one", open, len(tiers))
	}
}
