package firstlook

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retry runs once more the steps whose checks degraded with a transient
// error (prev.Retryable) and folds what they find into prev: the same
// report, each rerun check marked Retried with its first failure kept in
// its note. A check is retried only once, so the result has no Retryable.
func Retry(ctx context.Context, pool *pgxpool.Pool, opts Options,
	prev Report) (Report, error) {
	if pool == nil {
		return Report{}, ErrNoPool
	}
	if len(prev.Retryable) == 0 {
		return prev, nil
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("first look retry: %w", err)
	}
	opts = opts.withDefaults()
	fresh := &Report{}
	p := &pass{pool: pool, opts: opts, report: fresh}
	defer p.close()
	if err := p.openOrDegrade(ctx); err != nil {
		return Report{}, err
	}
	p.header(ctx)
	var rerun []string
	for _, st := range p.steps() {
		if !slices.ContainsFunc(st.rules, func(rule string) bool {
			return slices.Contains(prev.Retryable, rule)
		}) {
			continue
		}
		rerun = append(rerun, st.rules...)
		if err := p.run(ctx, st); err != nil {
			return Report{}, err
		}
	}
	return mergeRetry(prev, *fresh, rerun), nil
}

// mergeRetry folds the outcomes of the rerun rules into a copy of prev. A
// step that failed added no items, so the retry's items are added to prev's
// rather than replacing any. Facts were proposed after the first attempt,
// so only a rerun test-schema step brings new proposals.
func mergeRetry(prev, fresh Report, rerun []string) Report {
	out := prev
	out.Items = append(slices.Clone(prev.Items), fresh.Items...)
	sortItems(out.Items)
	again := make(map[string]Check, len(fresh.Checks))
	for _, c := range fresh.Checks {
		again[c.Rule] = c
	}
	out.Checks = slices.Clone(prev.Checks)
	for i, first := range out.Checks {
		c, ok := again[first.Rule]
		if !ok || !slices.Contains(rerun, first.Rule) {
			continue
		}
		c.Retried = true
		c.Note = joinNotes(c.Note, "first attempt: "+first.Note)
		out.Checks[i] = c
	}
	if slices.Contains(rerun, RuleMissingExtension) && fresh.Capabilities != nil {
		out.Capabilities = fresh.Capabilities
	}
	out.FactProposals = nil
	if slices.Contains(rerun, RuleTestSchema) {
		out.FactProposals = fresh.FactProposals
	}
	if out.Relations == 0 {
		out.Relations = fresh.Relations
	}
	out.Retryable = nil
	return out
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// retryableError reports whether a failed step may succeed when run again:
// a timeout, a lock or serialization conflict, a lost connection. A missing
// privilege or object fails the same way every time.
func retryableError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57014", "55P03", "40001", "40P01":
			return true
		}
		return false
	}
	return true
}
