package analyzer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// RuleDisabledResolution is the detail.resolution a finding resolved by a
// rule pack carries (market-11): it was not cleared, it was switched off.
const RuleDisabledResolution = "rule_disabled"

// globalPackSource names who disabled a rule when the global list did.
const globalPackSource = "config analyzer.disabled_rules"

// rulePackOverride is a per-database disabled-rules list (market-11) and
// who set it.
type rulePackOverride struct {
	names []string
	by    string
}

// rulePackState is the per-database override; nil inherits the global
// analyzer.disabled_rules. It is set by the API while the cycle reads it.
type rulePackState struct {
	override atomic.Pointer[rulePackOverride]
	// warned is the last invalid-name warning logged, so it is said once.
	warned atomic.Pointer[string]
}

// SetDisabledRules gives this database its own disabled-rules list, which
// replaces the global one; an empty list disables nothing here. by names
// who set it (the API user, or the config path).
func (a *Analyzer) SetDisabledRules(names []string, by string) {
	o := &rulePackOverride{names: append([]string{}, names...), by: by}
	a.rulePack.override.Store(o)
}

// ClearDisabledRules makes this database inherit the global list again.
func (a *Analyzer) ClearDisabledRules() { a.rulePack.override.Store(nil) }

// DisabledRulesOverride returns this database's own list, if it has one.
func (a *Analyzer) DisabledRulesOverride() ([]string, bool) {
	o := a.rulePack.override.Load()
	if o == nil {
		return nil, false
	}
	return slices.Clone(o.names), true
}

// disabledSet resolves the effective list. A name that cannot be disabled
// (a hand-edited override row) is ignored with one warning: the rule
// keeps running.
func (a *Analyzer) disabledSet() (findingcat.Disabled, string) {
	var names []string
	by := globalPackSource
	if a.cfg != nil {
		names = a.cfg.Analyzer.DisabledRules
	}
	if o := a.rulePack.override.Load(); o != nil {
		names, by = o.names, o.by
	}
	d, err := findingcat.ResolveDisabled(names)
	if err != nil {
		msg := err.Error()
		if last := a.rulePack.warned.Swap(&msg); last == nil || *last != msg {
			a.logFn("WARN", "analyzer: disabled rules ignored, they keep running: %v", err)
		}
	}
	return d, by
}

// disabledCategories returns the disabled categories and who disabled them.
func (a *Analyzer) disabledCategories() (map[string]bool, string) {
	d, by := a.disabledSet()
	return d.Categories, by
}

// cyclePack loads the effective rule pack into the cycle once.
func (a *Analyzer) cyclePack() *cycleEval {
	if a.eval != nil && !a.eval.packLoaded {
		d, by := a.disabledSet()
		a.eval.disabled, a.eval.disabledRules, a.eval.disabledBy = d.Categories, d.Rules, by
		a.eval.packLoaded = true
	}
	return a.eval
}

// ruleDisabled reports whether a snapshot rule is skipped this cycle: it
// is named, or every category it owns is disabled.
func (a *Analyzer) ruleDisabled(rule RuleSpec) bool {
	e := a.cyclePack()
	if e == nil {
		return false
	}
	if e.disabledRules[rule.Name] {
		return true
	}
	if len(rule.Categories) == 0 {
		return false
	}
	for _, c := range rule.Categories {
		if !e.disabled[c] {
			return false
		}
	}
	return true
}

// applyDisabledRules drops the findings of disabled categories. With
// nothing disabled it returns all unchanged.
func (a *Analyzer) applyDisabledRules(all []Finding) []Finding {
	e := a.cyclePack()
	if e == nil || len(e.disabled) == 0 {
		return all
	}
	out := all[:0:0]
	for _, f := range all {
		if !e.disabled[f.Category] {
			out = append(out, f)
		}
	}
	return out
}

// resolveDisabled resolves the open findings of every disabled category
// with resolution rule_disabled and who disabled it; they are never left
// open or dropped silently.
func (a *Analyzer) resolveDisabled(ctx context.Context) {
	if a.pool == nil || a.eval == nil {
		return
	}
	for cat := range a.eval.disabled {
		cleared, err := resolveDisabledCategory(ctx, a, cat, a.eval.disabledBy)
		a.dispatchResolvedCritical(ctx, cleared)
		if err != nil {
			a.logFn("ERROR", "analyzer: resolve disabled rule %s: %v", cat, err)
			continue
		}
		if len(cleared) > 0 {
			a.logFn("INFO", "analyzer: rule %s disabled by %s: resolved %d open findings",
				cat, a.eval.disabledBy, len(cleared))
		}
	}
}

func resolveDisabledCategory(
	ctx context.Context, a *Analyzer, category, by string,
) ([]ClearedFinding, error) {
	rows, err := a.pool.Query(ctx, `/* pg_sage analyzer */ UPDATE sage.findings
		SET status = 'resolved', resolved_at = now(),
		    detail = COALESCE(detail, '{}'::jsonb) ||
		             jsonb_build_object('resolution', $2::text, 'disabled_by', $3::text)
		WHERE category = $1 AND status = 'open'
		RETURNING category, COALESCE(object_identifier, ''), severity, title`,
		category, RuleDisabledResolution, by)
	if err != nil {
		return nil, fmt.Errorf("resolve open %s findings: %w", category, err)
	}
	cleared, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ClearedFinding, error) {
		var c ClearedFinding
		err := r.Scan(&c.Category, &c.ObjectIdentifier, &c.Severity, &c.Title)
		return c, err
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return cleared, fmt.Errorf("read resolved %s findings: %w", category, err)
	}
	return cleared, nil
}
