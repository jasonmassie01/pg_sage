package facts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// existsSQL checks that a fact's subject still names something, by kind.
// Patterns become LIKE patterns ('*' is '%', LIKE metacharacters escaped).
var existsSQL = map[Kind]string{
	KindSchema: `/* pg_sage facts */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace
		WHERE nspname LIKE $1 AND $2 = '')`,
	KindTable: `/* pg_sage facts */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname LIKE $1 AND c.relname LIKE $2 AND c.relkind IN ('r','p','m','f'))`,
	KindIndex: `/* pg_sage facts */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname LIKE $1 AND c.relname LIKE $2 AND c.relkind IN ('i','I'))`,
	KindSlot: `/* pg_sage facts */ SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_replication_slots
		WHERE slot_name LIKE $2 AND $1 = '')`,
}

// expiryDatePassed is the reason a fact past its expiry date expires.
const expiryDatePassed = "its expiry date passed"

// verifierActor is who re-verification's expiries are recorded as
// (expired_by and the fact history).
const verifierActor = "pg_sage:verifier"

// Reverify re-checks every proposed and confirmed fact at now. A fact past
// its expiry date expires; a fact whose subject no longer names anything
// is marked absent (missing_since, kept across restarts) and expires once
// the absence has lasted its grace (a migration may be recreating it): a
// week for a confirmed operator or model fact, which keeps binding
// meanwhile, an hour for proposals and detector facts. The others are
// marked verified and any absence cleared. Facts in fresh were just
// observed (re-proposed this pass) and count as present. Only a confirmed
// fact's expiry date, which a person accepted, unregisters a slot; an
// absence keeps it registered, so a recreated slot is still escalated. A
// fact that cannot be checked is reported and the others still are. It
// returns the facts it expired and the per-fact errors joined.
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
	var errs []error
	for _, f := range live {
		v, err := s.check(ctx, f, now, seen[f.ID])
		switch {
		case err != nil:
			errs = append(errs, err)
		case v.present:
			verified = append(verified, f.ID)
		case v.expired != nil:
			expired = append(expired, *v.expired)
		}
	}
	if len(verified) > 0 {
		if _, err := s.pool.Exec(ctx, `/* pg_sage facts */ UPDATE sage.facts
			SET last_verified_at = $2, missing_since = NULL
			WHERE id = ANY($1) AND status IN ('proposed', 'confirmed')`,
			verified, now); err != nil {
			errs = append(errs, fmt.Errorf("mark facts verified: %w", err))
		}
	}
	return expired, errors.Join(errs...)
}

// verdict is what re-verification found of one fact: its subject present,
// the fact expired, or neither (absent inside the grace, or another pass
// expired it first).
type verdict struct {
	present bool
	expired *Fact
}

func (s *Store) check(ctx context.Context, f Fact, now time.Time, fresh bool) (verdict,
	error) {
	if f.ExpiresAt != nil && !f.ExpiresAt.After(now) {
		return s.expireFor(ctx, f, expiryDatePassed, f.Status == StatusConfirmed, false)
	}
	if fresh || f.Kind == KindDatabase {
		// A database fact's subject is the database itself: always present,
		// with no catalog lookup.
		return verdict{present: true}, nil
	}
	p, err := ParsePattern(f.Kind, f.Subject)
	if err != nil {
		if f.Status == StatusConfirmed {
			// A person's decision is not dropped because the parser changed:
			// the fact stays confirmed until someone expires or corrects it.
			return verdict{}, fmt.Errorf("re-verify fact %d: its subject %q no longer "+
				"parses (kept confirmed; expire or re-declare it): %w", f.ID, f.Subject, err)
		}
		return s.expireFor(ctx, f, "its subject is no longer valid: "+err.Error(), false,
			false)
	}
	var exists bool
	if err := s.catalog.QueryRow(ctx, existsSQL[f.Kind], likePattern(p.Schema),
		likePattern(p.Name)).Scan(&exists); err != nil {
		return verdict{}, fmt.Errorf("re-verify fact %d (%s %s): %w", f.ID, f.Kind,
			f.Subject, err)
	}
	if exists {
		return verdict{present: true}, nil
	}
	since, ok, err := s.markMissing(ctx, f, now)
	if err != nil || !ok || now.Sub(since) < s.absenceGrace(f) {
		return verdict{}, err
	}
	noun := kindNames[f.Kind]
	if f.Kind == KindSchema {
		noun = "schema"
	}
	return s.expireFor(ctx, f, fmt.Sprintf("no %s matches %s any more (absent since %s)",
		noun, f.Subject, since.UTC().Format(time.RFC3339)), false, true)
}

// absenceGrace is how long f's subject may be absent before f expires: a
// confirmed operator or model fact is a person's decision and outlives an
// ordinary rebuild or connector reset; a proposal or a detector's fact is
// re-proposed by its detector when the subject is back.
func (s *Store) absenceGrace(f Fact) time.Duration {
	if f.Status == StatusConfirmed && f.Source != SourceDetector {
		return s.confirmedAbsence
	}
	return s.grace
}

// markMissing records the start of f's absence (once) and returns it; ok
// is false when f left the live statuses meanwhile.
func (s *Store) markMissing(ctx context.Context, f Fact, now time.Time) (time.Time, bool,
	error) {
	if f.MissingSince != nil {
		return *f.MissingSince, true, nil
	}
	var since time.Time
	err := s.pool.QueryRow(ctx, `/* pg_sage facts */ UPDATE sage.facts
		SET missing_since = COALESCE(missing_since, $2)
		WHERE id = $1 AND status IN ('proposed', 'confirmed')
		RETURNING missing_since`, f.ID, now).Scan(&since)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("mark fact %d absent: %w", f.ID, err)
	}
	return since, true, nil
}

// expireFor expires f. A fact another pass (or a person) already moved out
// of the live statuses is skipped: one expiry, one notification.
func (s *Store) expireFor(ctx context.Context, f Fact, reason string, unregister,
	byAbsence bool) (verdict, error) {
	e, err := s.expire(ctx, f.ID, verifierActor, reason, unregister, byAbsence)
	if errors.Is(err, ErrInvalidTransition) || errors.Is(err, ErrNotFound) {
		return verdict{}, nil
	}
	if err != nil {
		return verdict{}, err
	}
	return verdict{expired: &e}, nil
}
