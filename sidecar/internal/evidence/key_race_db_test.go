package evidence

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// audit-10: the race TestLoadOrCreateKey_ConcurrentCallersAgree rarely
// hits, forced: another sidecar creates the key after this one read an
// empty table and before it inserts.

// racingStore runs competitor once, right after the first read is scanned.
type racingStore struct {
	*pgxpool.Pool
	competitor func()
}

type racingRow struct {
	pgx.Row
	after func()
}

func (r racingRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.after()
	return err
}

func (s *racingStore) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := s.Pool.QueryRow(ctx, sql, args...)
	if s.competitor == nil {
		return row
	}
	run := s.competitor
	s.competitor = nil
	return racingRow{Row: row, after: run}
}

// Concurrent access: the loser of the insert race adopts the winner's key,
// does not report creating it, and leaves one row.
func TestLoadOrCreateKey_LosingTheInsertRaceAdoptsTheWinner(t *testing.T) {
	pool := freshDB(t, "evidence_key_race")
	ctx := context.Background()
	var winner StoredKey
	store := &racingStore{Pool: pool, competitor: func() {
		var err error
		if winner, err = LoadOrCreateKey(ctx, pool, testKeyring(t, 10)); err != nil {
			t.Errorf("competitor: %v", err)
		}
	}}
	got, err := LoadOrCreateKey(ctx, store, testKeyring(t, 10))
	if err != nil {
		t.Fatalf("the caller that lost the insert race failed: %v", err)
	}
	if !winner.Created || got.Created {
		t.Fatalf("created: winner %v, loser %v; want only the winner", winner.Created,
			got.Created)
	}
	if got.KeyID != winner.KeyID || !got.Key.Equal(winner.Key) {
		t.Fatalf("loser signs with %s, winner with %s", got.KeyID, winner.KeyID)
	}
	if row := readKeyRow(t, pool); row.count != 1 || row.keyID != winner.KeyID {
		t.Fatalf("row = %+v, want one row with key %s", row, winner.KeyID)
	}
}
