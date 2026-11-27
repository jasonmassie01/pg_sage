package changefeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Provider events (guard-02b): automatic minor upgrades, live migrations
// and forced failovers change a managed server under the workload. The
// server_version and server_host sources join the restart and failover
// sources; each such event is attributed to the operator when it follows
// an operator change within AttributionWindow, otherwise to the provider
// on a managed database and unknown when self-managed.

// Attributions of a provider event.
const (
	AttributionOperator = "operator"
	AttributionProvider = "provider"
	AttributionUnknown  = "unknown"
)

// AttributionWindow is how soon after an operator change an event is
// taken to be its consequence.
const AttributionWindow = 10 * time.Minute

// attributionLookback bounds the operator-change reads.
const attributionLookback = time.Hour

// errInvalidValue marks a source value that cannot be read as what it
// claims to be: the poll is ignored (state kept) with a logged reason.
var errInvalidValue = errors.New("invalid source value")

// providerSources are the sources that run even with feed polling off:
// cheap catalog reads of what a provider changes under the database.
func (p *Poller) providerSources() []source {
	out := []source{
		{name: "postmaster", poll: pollPostmaster},
		{name: "recovery_role", poll: pollRecoveryRole},
		{name: "timeline", poll: pollTimeline},
		{name: "server_version", poll: p.pollServerVersion},
		{name: "server_host", poll: pollServerHost},
	}
	if p.providerAPI != nil {
		out = append(out, source{name: SourceProviderAPI, poll: p.pollProviderAPI})
	}
	return out
}

// attributable reports whether events of kind carry an attribution.
func attributable(k Kind) bool {
	switch k {
	case KindRestart, KindFailover, KindVersionChange, KindHostChange:
		return true
	}
	return false
}

// attribution labels an event at `at` given the latest operator change at
// or before it (zero: none known).
func attribution(at, lastOperator time.Time, managed bool) string {
	if !lastOperator.IsZero() && !lastOperator.After(at) &&
		at.Sub(lastOperator) <= AttributionWindow {
		return AttributionOperator
	}
	if managed {
		return AttributionProvider
	}
	return AttributionUnknown
}

// attribute labels the attributable events. A failed operator-change
// read leaves that read out (logged); the label is never withheld.
func (p *Poller) attribute(ctx context.Context, events []Event) []Event {
	for i, e := range events {
		if !attributable(e.Kind) {
			continue
		}
		e.Attribution = attribution(e.OccurredAt, p.lastOperatorChange(ctx, e.OccurredAt),
			p.managed)
		e.Hash = hashOf(e)
		events[i] = e
	}
	return events
}

// lastOperatorChange is the latest operator change in the hour before
// at: a pg_sage setting change (sage.config_audit, this database's or
// fleet-wide) or a pg_sage config apply that awaits a restart.
func (p *Poller) lastOperatorChange(ctx context.Context, at time.Time) time.Time {
	type read struct {
		name string
		q    querier
		sql  string
		args []any
	}
	var reads []read
	if p.control != nil {
		reads = append(reads, read{"config_audit", p.control,
			`/* pg_sage sre:change_feed */ SELECT max(changed_at)
			FROM sage.config_audit WHERE changed_at <= $1 AND changed_at >= $2
			  AND (database_id IS NULL OR database_id = $3::int)`,
			[]any{at, at.Add(-attributionLookback), p.legacyID}})
	}
	if p.monitored != nil {
		reads = append(reads, read{"action_log", p.monitored,
			`/* pg_sage sre:change_feed */ SELECT max(executed_at)
			FROM sage.action_log WHERE outcome = 'applied_pending_restart'
			  AND executed_at <= $1 AND executed_at >= $2`,
			[]any{at, at.Add(-attributionLookback)}})
	}
	var latest time.Time
	for _, r := range reads {
		sctx, cancel := context.WithTimeout(ctx, sourceTimeout)
		var t *time.Time
		err := r.q.QueryRow(sctx, r.sql, r.args...).Scan(&t)
		cancel()
		if err != nil {
			p.logf("DEBUG", "sre change feed: attribution read of %s failed (%s): %v",
				r.name, classify(err), err)
			continue
		}
		if t != nil && t.After(latest) {
			latest = *t
		}
	}
	return latest
}

// versionState is the server_version source's snapshot: the version and
// the postmaster start it was read under.
type versionState struct {
	Num   int    `json:"num"`
	Start string `json:"start,omitempty"`
}

// parseVersionNum reads server_version_num (90624, 170005).
func parseVersionNum(raw string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%w: server_version_num is not a number", errInvalidValue)
	}
	if n < 10000 || n >= 10000000 {
		return 0, fmt.Errorf("%w: server_version_num %d is out of range", errInvalidValue, n)
	}
	return n, nil
}

// versionMajor is the major release of a version number: 17 for 170005,
// 906 (9.6) for 90624.
func versionMajor(n int) int {
	if n >= 100000 {
		return n / 10000
	}
	return n / 100
}

// formatVersion renders a version number as PostgreSQL prints it.
func formatVersion(n int) string {
	if n >= 100000 {
		return fmt.Sprintf("%d.%d", n/10000, n%10000)
	}
	return fmt.Sprintf("%d.%d.%d", n/10000, n/100%100, n%100)
}

// pollServerVersion snapshots server_version_num. A change is dated by
// the new postmaster start when the server restarted for it, otherwise by
// the poll.
func (p *Poller) pollServerVersion(ctx context.Context, q querier, prev []byte,
	now time.Time) ([]Event, any, error) {
	var raw string
	var start time.Time
	if err := q.QueryRow(ctx, `/* pg_sage sre:change_feed */
		SELECT current_setting('server_version_num'), pg_postmaster_start_time()`).
		Scan(&raw, &start); err != nil {
		return nil, nil, err
	}
	num, err := parseVersionNum(raw)
	if err != nil {
		p.logf("WARN", "sre change feed: server_version_num %q ignored: %v",
			truncate(raw, 32), err)
		return nil, nil, err
	}
	cur := versionState{Num: num, Start: start.UTC().Format(time.RFC3339Nano)}
	if prev == nil {
		return nil, cur, nil
	}
	var old versionState
	if err := json.Unmarshal(prev, &old); err != nil {
		return nil, nil, fmt.Errorf("server_version state: %w", err)
	}
	if old.Num == 0 || old.Num == num {
		return nil, cur, nil
	}
	at := now
	if old.Start != cur.Start {
		at = parseUTC(cur.Start, now)
	}
	return []Event{versionEvent(old.Num, num, cur.Start, at, now)}, cur, nil
}

func versionEvent(old, cur int, start string, at, now time.Time) Event {
	change, direction := "minor", "upgrade"
	if versionMajor(old) != versionMajor(cur) {
		change = "major"
	}
	if cur < old {
		direction = "downgrade"
	}
	return internalEvent(KindVersionChange,
		fmt.Sprintf("version:%s:%d->%d@%s", change, old, cur, start),
		fmt.Sprintf("PostgreSQL %s -> %s (%s version %s)", formatVersion(old),
			formatVersion(cur), change, direction), at, now)
}

// VersionChangeMajor reports whether e is a major version change.
func VersionChangeMajor(e Event) bool {
	return e.Kind == KindVersionChange && strings.HasPrefix(e.EventID, "version:major:")
}
