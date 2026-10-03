// Package pgssepoch lets a test that reads pg_stat_statements tell its own
// failure from interference: another package's test on the shared server
// can reset the statistics (pg_stat_statements_reset()) or evict entries
// while the measurement runs. Attempt repeats an attempt only when that
// happened, so a real failure is still reported at once.
package pgssepoch

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Querier is the subset of a pool or connection Epoch needs.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Epoch identifies the statistics' generation: a full reset changes
// stats_reset and an eviction raises dealloc.
func Epoch(ctx context.Context, q Querier) (string, error) {
	var epoch string
	err := q.QueryRow(ctx, `SELECT stats_reset::text || '/' || dealloc::text
		FROM pg_stat_statements_info`).Scan(&epoch)
	if err != nil {
		return "", fmt.Errorf("read pg_stat_statements_info: %w", err)
	}
	return epoch, nil
}

// Attempt runs try up to attempts times. try returns the problems it
// found; they fail the test when the statistics kept one generation
// across the attempt, and the attempt is repeated when they did not.
// Without pg_stat_statements_info (extension missing) the test is skipped.
func Attempt(t testing.TB, ctx context.Context, q Querier, attempts int,
	try func() []string) {
	t.Helper()
	res := run(func() (string, error) { return Epoch(ctx, q) }, attempts, try)
	for _, note := range res.notes {
		t.Log(note)
	}
	switch {
	case res.unavailable != nil:
		t.Skipf("pg_stat_statements unavailable: %v", res.unavailable)
	case res.err != nil:
		t.Fatal(res.err)
	}
	for _, p := range res.problems {
		t.Error(p)
	}
}

type result struct {
	problems    []string
	notes       []string
	unavailable error
	err         error
}

func run(epoch func() (string, error), attempts int, try func() []string) result {
	var res result
	var last []string
	for i := 1; i <= attempts; i++ {
		before, err := epoch()
		if err != nil {
			res.unavailable = err
			return res
		}
		problems := try()
		after, err := epoch()
		if err != nil {
			res.err = err
			return res
		}
		if len(problems) == 0 {
			return res
		}
		if before == after {
			res.problems = problems
			return res
		}
		last = problems
		res.notes = append(res.notes, fmt.Sprintf("attempt %d: pg_stat_statements was "+
			"reset or evicted during the measurement (%s -> %s); repeating", i, before, after))
	}
	res.err = fmt.Errorf("pg_stat_statements was reset or evicted during each of %d "+
		"attempts; last problems: %v", attempts, last)
	return res
}
