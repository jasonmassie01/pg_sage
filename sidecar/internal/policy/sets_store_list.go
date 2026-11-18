package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Members lists the databases that follow set name (an exception against
// it included), by database id. An unknown set is ErrNotFound.
func (s *Store) Members(ctx context.Context, name string) ([]Resolution, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	out := make([]Resolution, 0)
	err := s.readSnapshot(ctx, nil, func(snap setSnapshot) error {
		if !snap.hasSet(name) {
			return fmt.Errorf("%w: policy set %q", ErrNotFound, name)
		}
		for _, m := range snap.members {
			res, err := snap.resolve(m.DatabaseID, s.clock())
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if res.Set == name {
				out = append(out, res)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (snap setSnapshot) hasSet(name string) bool {
	for _, s := range snap.sets {
		if s.Set.Name == name {
			return true
		}
	}
	return false
}

func (snap setSnapshot) setPolicy(name string) *Policy {
	for _, s := range snap.sets {
		if s.Set.Name == name {
			return s.Policy
		}
	}
	return nil
}

// Exceptions lists every database whose own document is in force, with
// its diff against the set it matches now, by database id.
func (s *Store) Exceptions(ctx context.Context) ([]Exception, error) {
	return s.exceptions(ctx, nil)
}

// ExceptionOf is databaseID's exception in force with its diff, or nil
// when the database follows its set (fleetpol-01c: one inbox read per
// database reads only that database's rows).
func (s *Store) ExceptionOf(ctx context.Context, databaseID int64) (*Exception, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	if databaseID <= 0 {
		return nil, fmt.Errorf("%w: database_id must be positive, got %d", ErrInvalidSet,
			databaseID)
	}
	got, err := s.exceptions(ctx, &databaseID)
	if err != nil {
		return nil, err
	}
	for i := range got {
		if got[i].DatabaseID == databaseID {
			return &got[i], nil
		}
	}
	return nil, nil
}

func (s *Store) exceptions(ctx context.Context, only *int64) ([]Exception, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	out := make([]Exception, 0)
	err := s.readSnapshot(ctx, only, func(snap setSnapshot) error {
		for id := range snap.own {
			res, err := snap.resolve(id, s.clock())
			if err != nil {
				return err
			}
			if res.Exception == nil {
				continue
			}
			exc := *res.Exception
			if set := snap.setPolicy(res.Set); set != nil {
				if exc.Diff, err = DocumentDiff(set.Document, res.Policy.Document); err != nil {
					return fmt.Errorf("diff database %d against set %s: %w", id, res.Set, err)
				}
			}
			out = append(out, exc)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DatabaseID < out[j].DatabaseID })
	return out, nil
}
