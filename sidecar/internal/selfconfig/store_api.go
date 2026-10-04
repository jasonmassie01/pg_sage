package selfconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Get returns one key's derivation state.
func (s *Store) Get(ctx context.Context, key string) (State, error) {
	if s == nil || s.pool == nil {
		return State{}, ErrUnavailable
	}
	states, err := loadStates(ctx, s.pool, key)
	if err != nil {
		return State{}, err
	}
	st, ok := states[key]
	if !ok || key == "" {
		return State{}, ErrNotFound
	}
	return st, nil
}

// History returns a key's newest ledger entries, newest first.
func (s *Store) History(ctx context.Context, key string, limit int) ([]LedgerEntry, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("selfconfig: history limit must be 1-1000, got %d", limit)
	}
	rows, err := s.pool.Query(ctx, `SELECT id, key, event, value, previous_value, evidence,
		bounds, rule, rule_version, reason, actor, created_at
		FROM sage.config_derivation WHERE key = $1
		ORDER BY created_at DESC, id DESC LIMIT $2`, key, limit)
	if err != nil {
		return nil, fmt.Errorf("selfconfig: read the ledger of %s: %w", key, err)
	}
	defer rows.Close()
	out := []LedgerEntry{}
	for rows.Next() {
		var e LedgerEntry
		var evidence, bounds []byte
		if err := rows.Scan(&e.ID, &e.Key, &e.Event, &e.Value, &e.Previous, &evidence,
			&bounds, &e.Rule, &e.RuleVersion, &e.Reason, &e.Actor, &e.At); err != nil {
			return nil, fmt.Errorf("selfconfig: scan the ledger of %s: %w", key, err)
		}
		if err := errors.Join(json.Unmarshal(evidence, &e.Evidence),
			json.Unmarshal(bounds, &e.Bounds)); err != nil {
			return nil, fmt.Errorf("selfconfig: decode the ledger of %s: %w", key, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("selfconfig: read the ledger of %s: %w", key, err)
	}
	return out, nil
}

// List returns every derived key's state with its newest ledger entries.
func (s *Store) List(ctx context.Context, history int) ([]Setting, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	states, err := loadStates(ctx, s.pool, "")
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(states))
	for k := range states {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Setting, 0, len(keys))
	for _, k := range keys {
		h, err := s.History(ctx, k, history)
		if err != nil {
			return nil, err
		}
		out = append(out, Setting{State: states[k], History: h})
	}
	return out, nil
}

// Pin freezes the value in force: evidence no longer changes it until
// Unpin. Pinning a pinned key is a no-op.
func (s *Store) Pin(ctx context.Context, key, actor string) (State, error) {
	return s.changePin(ctx, key, actor, true)
}

// Unpin lets derivation resume from the next pass.
func (s *Store) Unpin(ctx context.Context, key, actor string) (State, error) {
	return s.changePin(ctx, key, actor, false)
}

func (s *Store) changePin(ctx context.Context, key, actor string, pin bool) (State, error) {
	if _, ok := Lookup(key); !ok {
		return State{}, ErrUnknownKey
	}
	if actor == "" {
		return State{}, errors.New("selfconfig: a pin needs an actor")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	states, err := loadStates(ctx, tx, key)
	if err != nil {
		return State{}, err
	}
	st, ok := states[key]
	switch {
	case !ok:
		return State{}, ErrNotFound
	case st.Status == StatusOperator:
		return State{}, ErrOperatorSet
	case pin && st.Pinned != nil:
		return st, nil
	case !pin && st.Pinned == nil:
		return State{}, ErrNotPinned
	}
	now := time.Now()
	event := applyPin(&st, pin, actor, now)
	if err := saveState(ctx, tx, st); err != nil {
		return State{}, err
	}
	if err := appendEvents(ctx, tx, st, []Event{event}, actor, now); err != nil {
		return State{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return State{}, fmt.Errorf("selfconfig: commit the pin of %s: %w", key, err)
	}
	return st, nil
}

func applyPin(st *State, pin bool, actor string, now time.Time) Event {
	st.UpdatedAt = now
	if pin {
		st.Pinned, st.PinnedBy, st.PinnedAt = ptr(st.Value), actor, now
		st.Pending = nil
		st.clearShadow()
		st.Status = st.status()
		return Event{Kind: EventPinned, Value: ptr(st.Value), Previous: ptr(st.Value),
			Reason: "pinned the value in force; evidence no longer changes it"}
	}
	pinned := st.Pinned
	st.Pinned, st.PinnedBy, st.PinnedAt = nil, "", time.Time{}
	st.Status = st.status()
	return Event{Kind: EventUnpinned, Value: ptr(st.Value), Previous: pinned,
		Reason: "unpinned; derivation resumes on the next pass"}
}
