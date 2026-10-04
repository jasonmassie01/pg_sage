package facts

import (
	"context"
	"fmt"
	"time"
)

// existsSQL checks that a fact's subject still names something, by kind.
// Patterns become LIKE patterns ('*' is '%', LIKE metacharacters escaped).
var existsSQL = map[Kind]string{
	KindSchema: `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace
		WHERE nspname LIKE $1 AND $2 = '')`,
	KindTable: `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname LIKE $1 AND c.relname LIKE $2 AND c.relkind IN ('r','p','m','f'))`,
	KindIndex: `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname LIKE $1 AND c.relname LIKE $2 AND c.relkind IN ('i','I'))`,
	KindSlot: `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_replication_slots
		WHERE slot_name LIKE $2 AND $1 = '')`,
}

// Reverify re-checks every proposed and confirmed fact at now. A fact past
// its expiry date expires; a fact whose subject no longer names anything
// expires once that absence has lasted the grace period (a migration may
// be recreating it); the others are marked verified. Facts in fresh were
// just observed (re-proposed this pass) and count as present. It returns
// the facts it expired.
func (s *Store) Reverify(ctx context.Context, now time.Time, fresh ...int64) ([]Fact,
	error) {
	live, err := s.List(ctx, Filter{Status: []Status{StatusProposed, StatusConfirmed}})
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]bool, len(fresh))
	for _, id := range fresh {
		seen[id] = true
	}
	var expired []Fact
	var verified []int64
	for _, f := range live {
		reason, err := "", error(nil)
		if !seen[f.ID] || (f.ExpiresAt != nil && !f.ExpiresAt.After(now)) {
			reason, err = s.absence(ctx, f, now)
		}
		if err != nil {
			return expired, err
		}
		if reason == "" {
			verified = append(verified, f.ID)
			continue
		}
		if reason == "pending" {
			continue
		}
		e, err := s.Expire(ctx, f.ID, reason)
		if err != nil {
			return expired, err
		}
		s.forgetMissing(f.ID)
		expired = append(expired, e)
	}
	if len(verified) > 0 {
		if _, err := s.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.facts
			SET last_verified_at = $2 WHERE id = ANY($1)`, verified, now); err != nil {
			return expired, fmt.Errorf("mark facts verified: %w", err)
		}
	}
	return expired, nil
}

// absence is why f should expire at now: "" when its subject exists,
// "pending" while an absence is inside the grace, else the reason.
func (s *Store) absence(ctx context.Context, f Fact, now time.Time) (string, error) {
	if f.ExpiresAt != nil && !f.ExpiresAt.After(now) {
		return "its expiry date passed", nil
	}
	p, err := ParsePattern(f.Kind, f.Subject)
	if err != nil {
		return "its subject is no longer valid: " + err.Error(), nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, existsSQL[f.Kind], likePattern(p.Schema),
		likePattern(p.Name)).Scan(&exists); err != nil {
		return "", fmt.Errorf("re-verify fact %d: %w", f.ID, err)
	}
	if exists {
		s.forgetMissing(f.ID)
		return "", nil
	}
	since := s.markMissing(f.ID, now)
	if now.Sub(since) < s.grace {
		return "pending", nil
	}
	noun := kindNames[f.Kind]
	if f.Kind == KindSchema {
		noun = "schema"
	}
	return fmt.Sprintf("no %s matches %s any more (absent since %s)", noun,
		f.Subject, since.UTC().Format(time.RFC3339)), nil
}

func (s *Store) markMissing(id int64, now time.Time) time.Time {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	since, ok := s.shared.missing[id]
	if !ok {
		s.shared.missing[id] = now
		return now
	}
	return since
}

func (s *Store) forgetMissing(id int64) {
	s.shared.mu.Lock()
	defer s.shared.mu.Unlock()
	delete(s.shared.missing, id)
}
