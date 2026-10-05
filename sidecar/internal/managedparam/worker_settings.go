package managedparam

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// settingNames are the settings a cycle reads: the intents' parameters
// and the target's operator-set parameters (for drift).
func settingNames(intents []findingIntent, target Target) []string {
	set := map[string]bool{}
	for _, fi := range intents {
		set[fi.intent.Parameter] = true
	}
	for name := range configuredValues(target) {
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (w *Worker) readSettings(ctx context.Context,
	names []string) (map[string]collector.PGSetting, error) {
	out := map[string]collector.PGSetting{}
	if len(names) == 0 {
		return out, nil
	}
	rows, err := w.opts.Pool.Query(ctx, `/* pg_sage */ SELECT name, setting,
		COALESCE(unit, ''), source, pending_restart, context
		FROM pg_settings WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("read pg_settings for managed changes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s collector.PGSetting
		if err := rows.Scan(&s.Name, &s.Setting, &s.Unit, &s.Source, &s.PendingRestart,
			&s.Context); err != nil {
			return nil, fmt.Errorf("scan pg_settings: %w", err)
		}
		out[s.Name] = s
	}
	return out, rows.Err()
}

func settingList(m map[string]collector.PGSetting) []collector.PGSetting {
	out := make([]collector.PGSetting, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	return out
}

// appliedExists reports a proposal with the fingerprint already observed
// applied, so a value that keeps running is not proposed again.
func (s *Store) appliedExists(ctx context.Context, fingerprint string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sage.managed_change_proposals
		WHERE fingerprint = $1 AND status = 'applied')`, fingerprint).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read applied managed change: %w", err)
	}
	return exists, nil
}

// Run cycles every interval until ctx ends; report receives each outcome.
func (w *Worker) Run(ctx context.Context, interval time.Duration,
	report func(CycleResult, error)) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		res, err := w.Cycle(ctx)
		if ctx.Err() != nil {
			return
		}
		if report != nil {
			report(res, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
