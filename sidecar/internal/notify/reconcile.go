package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// ManagedByConfig marks the channels and rules the alerting reconciler
// owns (alerting-06b); the API writes "api" rows and never changes these.
const ManagedByConfig = "config"

// configChannelPrefix starts the name of every config-managed channel.
const configChannelPrefix = "config:"

// ConfigChannelName is the channel a YAML target becomes:
// config:<kind>:<name>, such as config:webhook:ops.
func ConfigChannelName(kind, name string) string {
	return configChannelPrefix + kind + ":" + name
}

// IsConfigChannelName reports whether name is reserved for the reconciler.
func IsConfigChannelName(name string) bool {
	return strings.HasPrefix(name, configChannelPrefix)
}

// reconciledEvents are the rules each routed channel gets: the finding
// alert, at the route's severity, and its resolve, so a PagerDuty
// incident opened by a finding is closed by it.
var reconciledEvents = []string{"finding_opened", "finding_resolved"}

// ManagedChannel is one config-managed channel row. Config is plaintext;
// the store seals its secrets. Hash identifies the config it came from.
type ManagedChannel struct {
	ID     int
	Name   string
	Type   string
	Config map[string]string
	Hash   string
}

// ManagedRule is one config-managed rule row. Hash covers the channel
// name, event and min_severity.
type ManagedRule struct {
	ID          int
	ChannelID   int
	Event       string
	MinSeverity string
	Hash        string
}

// ReconcileTx is the write surface of one reconcile transaction; every
// call reads or writes only managed_by = 'config' rows.
type ReconcileTx interface {
	ManagedChannels(ctx context.Context) ([]ManagedChannel, error)
	ManagedRules(ctx context.Context) ([]ManagedRule, error)
	InsertChannel(ctx context.Context, ch ManagedChannel) (int, error)
	UpdateChannel(ctx context.Context, ch ManagedChannel) error
	DeleteChannel(ctx context.Context, ch ManagedChannel) error
	InsertRule(ctx context.Context, r ManagedRule) (int, error)
	DeleteRule(ctx context.Context, r ManagedRule) error
}

// ReconcileStore runs fn in one transaction that two sidecars sharing a
// control database never run at once; an error from fn rolls it back.
type ReconcileStore interface {
	InReconcileTx(ctx context.Context, fn func(ReconcileTx) error) error
}

// ReconcileResult counts the rows one reconcile wrote; all zero means the
// tables already matched the block.
type ReconcileResult struct {
	ChannelsCreated int
	ChannelsUpdated int
	ChannelsDeleted int
	RulesCreated    int
	RulesDeleted    int
}

// desiredRule is a rule the block asks for, by channel name.
type desiredRule struct {
	channel     string
	event       string
	minSeverity string
	hash        string
}

// Reconcile makes the config-managed channels and rules match the
// alerting block a: one channel per usable target, one rule per routed
// channel and reconciled event. Rows created through the API are never
// read or written. A disabled or absent block deletes every config row.
func Reconcile(
	ctx context.Context, store ReconcileStore, a config.AlertingConfig,
	policy TargetPolicy, logFn func(string, string, ...any),
) (ReconcileResult, error) {
	if store == nil {
		return ReconcileResult{}, errors.New("reconcile alerting: no notification store")
	}
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	channels, rules := desiredState(a, policy, logFn)
	var res ReconcileResult
	err := store.InReconcileTx(ctx, func(tx ReconcileTx) error {
		res = ReconcileResult{}
		ids, err := syncChannels(ctx, tx, channels, &res)
		if err != nil {
			return err
		}
		return syncRules(ctx, tx, ids, rules, &res)
	})
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("reconcile alerting: %w", err)
	}
	return res, nil
}

// syncChannels deletes stale config channels (their rules first), updates
// changed ones and inserts new ones; it returns the id of each desired
// channel by name.
func syncChannels(
	ctx context.Context, tx ReconcileTx, want []ManagedChannel, res *ReconcileResult,
) (map[string]int, error) {
	have, err := tx.ManagedChannels(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading config channels: %w", err)
	}
	wanted := make(map[string]ManagedChannel, len(want))
	for _, ch := range want {
		wanted[ch.Name] = ch
	}
	ids := make(map[string]int, len(want))
	for _, cur := range have {
		next, keep := wanted[cur.Name]
		if _, dup := ids[cur.Name]; !keep || dup {
			if err := deleteChannel(ctx, tx, cur, res); err != nil {
				return nil, err
			}
			continue
		}
		ids[cur.Name] = cur.ID
		if cur.Hash == next.Hash && cur.Type == next.Type {
			continue
		}
		next.ID = cur.ID
		if err := tx.UpdateChannel(ctx, next); err != nil {
			return nil, fmt.Errorf("writing channel %q: %w", cur.Name, err)
		}
		res.ChannelsUpdated++
	}
	for _, ch := range want {
		if _, ok := ids[ch.Name]; ok {
			continue
		}
		id, err := tx.InsertChannel(ctx, ch)
		if err != nil {
			return nil, fmt.Errorf("writing channel %q: %w", ch.Name, err)
		}
		ids[ch.Name] = id
		res.ChannelsCreated++
	}
	return ids, nil
}

// deleteChannel removes a config channel and, first, its config rules, so
// the counts say what went.
func deleteChannel(
	ctx context.Context, tx ReconcileTx, ch ManagedChannel, res *ReconcileResult,
) error {
	rules, err := tx.ManagedRules(ctx)
	if err != nil {
		return fmt.Errorf("reading config rules: %w", err)
	}
	for _, r := range rules {
		if r.ChannelID != ch.ID {
			continue
		}
		if err := tx.DeleteRule(ctx, r); err != nil {
			return fmt.Errorf("deleting rule %d of channel %q: %w", r.ID, ch.Name, err)
		}
		res.RulesDeleted++
	}
	if err := tx.DeleteChannel(ctx, ch); err != nil {
		return fmt.Errorf("writing channel %q: %w", ch.Name, err)
	}
	res.ChannelsDeleted++
	return nil
}

// syncRules deletes config rules the block no longer asks for (or that
// repeat one it does) and inserts the missing ones.
func syncRules(
	ctx context.Context, tx ReconcileTx, ids map[string]int,
	want []desiredRule, res *ReconcileResult,
) error {
	have, err := tx.ManagedRules(ctx)
	if err != nil {
		return fmt.Errorf("reading config rules: %w", err)
	}
	wanted := make(map[string]bool, len(want))
	for _, r := range want {
		wanted[r.hash] = true
	}
	kept := make(map[string]bool, len(have))
	for _, r := range have {
		if wanted[r.Hash] && !kept[r.Hash] {
			kept[r.Hash] = true
			continue
		}
		if err := tx.DeleteRule(ctx, r); err != nil {
			return fmt.Errorf("deleting rule %d: %w", r.ID, err)
		}
		res.RulesDeleted++
	}
	for _, r := range want {
		if kept[r.hash] {
			continue
		}
		rule := ManagedRule{ChannelID: ids[r.channel], Event: r.event,
			MinSeverity: r.minSeverity, Hash: r.hash}
		if _, err := tx.InsertRule(ctx, rule); err != nil {
			return fmt.Errorf("writing %s rule of channel %q: %w", r.event, r.channel, err)
		}
		res.RulesCreated++
	}
	return nil
}

// desiredState is what the block asks for: the usable targets as
// channels, sorted by name, and their rules.
func desiredState(
	a config.AlertingConfig, policy TargetPolicy, logFn func(string, string, ...any),
) ([]ManagedChannel, []desiredRule) {
	if !a.Enabled {
		return nil, nil
	}
	byKey := targetChannels(a, policy, logFn)
	minSev := routedSeverities(a, byKey, logFn)
	channels := make([]ManagedChannel, 0, len(byKey))
	for _, ch := range byKey {
		channels = append(channels, ch)
	}
	sort.Slice(channels, func(i, j int) bool { return channels[i].Name < channels[j].Name })
	var rules []desiredRule
	for _, ch := range channels {
		sev, routed := minSev[ch.Name]
		if !routed {
			continue
		}
		for _, ev := range reconciledEvents {
			rules = append(rules, desiredRule{channel: ch.Name, event: ev, minSeverity: sev,
				hash: hashOf(ch.Name, ev, sev)})
		}
	}
	return channels, rules
}

// routedSeverities is, per channel name, the lowest severity a route
// sends it: notify rules match "at least min_severity", and one rule per
// channel and event keeps a finding to one post per channel.
func routedSeverities(
	a config.AlertingConfig, byKey map[string]ManagedChannel,
	logFn func(string, string, ...any),
) map[string]string {
	out := map[string]string{}
	for _, r := range a.Routes {
		if _, ok := ValidSeverities[r.Severity]; !ok {
			logFn("WARN", "alerting: route severity %q is not critical, warning or info; "+
				"skipped", r.Severity)
			continue
		}
		for _, name := range r.Channels {
			key, ok := a.ResolveAlertChannel(name)
			if !ok {
				logFn("WARN", "alerting: %s route channel %q is not configured; nothing "+
					"is sent to it", r.Severity, name)
				continue
			}
			ch, built := byKey[key]
			if !built {
				continue // refused by the target policy, logged there
			}
			if cur, seen := out[ch.Name]; !seen || ValidSeverities[r.Severity] <
				ValidSeverities[cur] {
				out[ch.Name] = r.Severity
			}
		}
	}
	return out
}

// hashOf is the hex SHA-256 of parts joined by a separator no part holds.
func hashOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// channelHash identifies a channel's type and plaintext config.
func channelHash(typ string, cfg map[string]string) string {
	body, _ := json.Marshal(cfg) // a string map always marshals
	return hashOf(typ, string(body))
}
