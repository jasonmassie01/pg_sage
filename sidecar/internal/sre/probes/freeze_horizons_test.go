package probes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// failingQuerier fails every query, counting them.
type failingQuerier struct {
	err   error
	calls int
}

func (q *failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	q.calls++
	return nil, q.err
}

func (q *failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

func TestFreezeHorizonShare(t *testing.T) {
	cases := []struct {
		name string
		h    FreezeHorizon
		want float64
	}{
		{"xid", FreezeHorizon{XIDAge: 75, XIDMaxAge: 100, MultiXactMaxAge: 100}, 0.75},
		{"multixact larger", FreezeHorizon{XIDAge: 10, XIDMaxAge: 100,
			MultiXactAge: 90, MultiXactMaxAge: 100}, 0.9},
		{"unknown maxima", FreezeHorizon{XIDAge: 10, MultiXactAge: 10}, 0},
		{"zero", FreezeHorizon{}, 0},
		{"past the maximum", FreezeHorizon{XIDAge: 150, XIDMaxAge: 100}, 1.5},
	}
	for _, c := range cases {
		if got := c.h.Share(); got != c.want {
			t.Errorf("%s: Share() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestReadFreezeHorizonsRejectsUnboundedOrNegativeArguments(t *testing.T) {
	q := &failingQuerier{err: errors.New("unreachable")}
	for _, limit := range []int{0, -1} {
		if _, err := ReadFreezeHorizons(context.Background(), q, limit, 0); err == nil ||
			!strings.Contains(err.Error(), "limit") {
			t.Errorf("limit %d: err = %v, want a limit error", limit, err)
		}
	}
	if _, err := ReadFreezeHorizons(context.Background(), q, 10, -1); err == nil ||
		!strings.Contains(err.Error(), "bloat floor") {
		t.Errorf("negative floor: err = %v, want a bloat floor error", err)
	}
	if q.calls != 0 {
		t.Fatalf("invalid arguments reached the database %d times", q.calls)
	}
}

func TestReadFreezeHorizonsPropagatesQueryErrors(t *testing.T) {
	q := &failingQuerier{err: errors.New("connection refused")}
	horizons, err := ReadFreezeHorizons(context.Background(), q, 10, 0)
	if err == nil || !strings.Contains(err.Error(), "read freeze horizons") ||
		!strings.Contains(err.Error(), "connection refused") || horizons != nil {
		t.Fatalf("ReadFreezeHorizons = %v, %v; want the wrapped connection error", horizons, err)
	}
}

// The reader and the probe rank with one CTE text: the custodian and the
// SRE probe cannot drift apart again.
func TestFreezeHorizonsShareTheProbeRanking(t *testing.T) {
	for name, sql := range map[string]string{"wraparound_tables": wraparoundTablesSQL,
		"freeze_horizons": FreezeHorizonsSQL} {
		if !strings.Contains(sql, freezeRankCTE) {
			t.Errorf("%s does not use the shared ranking", name)
		}
	}
	for _, want := range []string{"LIMIT $1", "reltoastrelid", "relisshared",
		"relfrozenxid <> '0'::xid"} {
		if !strings.Contains(FreezeHorizonsSQL, want) {
			t.Errorf("FreezeHorizonsSQL lacks %q", want)
		}
	}
	if strings.Contains(FreezeHorizonsSQL, "pg_stat_user_tables") ||
		strings.Contains(FreezeHorizonsSQL, "pg_stat_all_tables") {
		t.Fatal("FreezeHorizonsSQL joins a statistics view for every relation")
	}
}
