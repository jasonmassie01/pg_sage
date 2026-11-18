package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// setSnapshot is everything resolution reads, from one transaction: the
// sets with their documents, the members, their own documents and their
// exception metadata.
type setSnapshot struct {
	sets       []SetDocument
	members    []Member
	own        map[int64]*Policy
	exceptions map[int64]*Exception
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

const policySetsSQL = `/* pg_sage policy_sets v1 */
SELECT s.name, s.priority, s.selectors, s.requirements, s.created_by, s.updated_at,
       p.id, p.version, p.profile, p.doc, COALESCE(p.ratified_by, p.proposed_by),
       COALESCE(p.activated_at, p.ratified_at, p.proposed_at)
FROM sage.policy_sets s
LEFT JOIN sage.policy p
  ON p.set_name = s.name AND p.database_id IS NULL AND p.status = 'active'
ORDER BY s.priority DESC, s.name`

// Resolve is the document in force for databaseID, read in one read-only
// repeatable-read transaction so a concurrent set change is seen whole or
// not at all. A read failure is an error, never a fallback document.
func (s *Store) Resolve(ctx context.Context, databaseID int64) (Resolution, error) {
	if s == nil || s.pool == nil {
		return Resolution{}, ErrUnavailable
	}
	var out Resolution
	err := s.readSnapshot(ctx, &databaseID, func(snap setSnapshot) error {
		var err error
		out, err = snap.resolve(databaseID, s.clock())
		return err
	})
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrSetConflict) {
		return Resolution{}, fmt.Errorf("resolve policy for database %d: %w", databaseID, err)
	}
	return out, err
}

// readSnapshot reads a snapshot (of one database, or all when only is nil)
// and hands it to use inside the transaction.
func (s *Store) readSnapshot(
	ctx context.Context, only *int64, use func(setSnapshot) error,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("begin policy read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	snap, err := loadSnapshot(ctx, tx, only)
	if err != nil {
		return err
	}
	if err := use(snap); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadSnapshot(ctx context.Context, q querier, only *int64) (setSnapshot, error) {
	var snap setSnapshot
	var err error
	if snap.sets, err = readSets(ctx, q); err != nil {
		return setSnapshot{}, err
	}
	if snap.members, err = readMembers(ctx, q, only); err != nil {
		return setSnapshot{}, err
	}
	if snap.own, err = readOwnDocuments(ctx, q, only); err != nil {
		return setSnapshot{}, err
	}
	if snap.exceptions, err = readExceptions(ctx, q, only); err != nil {
		return setSnapshot{}, err
	}
	return snap, nil
}

// member is databaseID's member, or a bare one (no tags) when it is not
// registered in sage.databases.
func (snap setSnapshot) member(databaseID int64) Member {
	for _, m := range snap.members {
		if m.DatabaseID == databaseID {
			return m
		}
	}
	return Member{DatabaseID: databaseID, Tags: map[string]string{}}
}

func (snap setSnapshot) resolve(databaseID int64, now time.Time) (Resolution, error) {
	return Resolve(snap.sets, snap.member(databaseID), snap.own[databaseID],
		snap.exceptions[databaseID], now)
}

// Sets lists the policy sets with their active documents, by priority.
func (s *Store) Sets(ctx context.Context) ([]SetDocument, error) {
	if s == nil || s.pool == nil {
		return nil, ErrUnavailable
	}
	return readSets(ctx, s.pool)
}

func readSets(ctx context.Context, q querier) ([]SetDocument, error) {
	rows, err := q.Query(ctx, policySetsSQL)
	if err != nil {
		return nil, fmt.Errorf("read policy sets: %w", err)
	}
	defer rows.Close()
	out := make([]SetDocument, 0)
	for rows.Next() {
		item, err := scanSetDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policy sets: %w", err)
	}
	return out, nil
}

func scanSetDocument(rows pgx.Rows) (SetDocument, error) {
	var set PolicySet
	var selectors, requirements []byte
	var id, version *int64
	var profile, by *string
	var doc []byte
	var at *time.Time
	if err := rows.Scan(&set.Name, &set.Priority, &selectors, &requirements, &set.CreatedBy,
		&set.UpdatedAt, &id, &version, &profile, &doc, &by, &at); err != nil {
		return SetDocument{}, fmt.Errorf("scan policy set: %w", err)
	}
	if err := json.Unmarshal(selectors, &set.Selectors); err != nil {
		return SetDocument{}, fmt.Errorf("policy set %s: selectors: %w", set.Name, err)
	}
	if err := json.Unmarshal(requirements, &set.Requirements); err != nil {
		return SetDocument{}, fmt.Errorf("policy set %s: requirements: %w", set.Name, err)
	}
	out := SetDocument{Set: set}
	if id != nil {
		out.Policy = &Policy{ID: *id, Version: *version, Profile: *profile,
			Document: doc, UpdatedBy: *by, Scope: Scope{SetName: set.Name},
			Set: set.Name}
		if at != nil {
			out.Policy.UpdatedAt = *at
		}
	}
	return out, nil
}

// readMembers reads what selectors see of each registered database; a
// discovered member's creator is its discovery row's (fleetpol-01b).
func readMembers(ctx context.Context, q querier, only *int64) ([]Member, error) {
	rows, err := q.Query(ctx, `/* pg_sage policy_set_members v1 */
SELECT m.id, m.name, COALESCE(m.tags, '{}'::jsonb), COALESCE(m.discovered_source, ''),
       COALESCE((SELECT d.creator FROM sage.discovered_databases d
                  WHERE d.database_id = m.id ORDER BY d.id DESC LIMIT 1), '')
FROM sage.databases m WHERE $1::bigint IS NULL OR m.id = $1 ORDER BY m.id`, only)
	if err != nil {
		return nil, fmt.Errorf("read policy set members: %w", err)
	}
	defer rows.Close()
	out := make([]Member, 0)
	for rows.Next() {
		var m Member
		var tags []byte
		if err := rows.Scan(&m.DatabaseID, &m.Name, &tags, &m.DiscoverySource,
			&m.Creator); err != nil {
			return nil, fmt.Errorf("scan policy set member: %w", err)
		}
		if m.Tags, err = decodeMemberTags(tags); err != nil {
			return nil, fmt.Errorf("database %d tags: %w", m.DatabaseID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policy set members: %w", err)
	}
	return out, nil
}

// decodeMemberTags reads sage.databases.tags: an object of strings, or a
// list of "key=value" strings (a bare key has the value "").
func decodeMemberTags(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err == nil {
		return out, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("want an object of strings or a list of key=value: %w", err)
	}
	for _, item := range list {
		key, value, _ := strings.Cut(item, "=")
		out[key] = value
	}
	return out, nil
}

func readOwnDocuments(ctx context.Context, q querier, only *int64) (map[int64]*Policy, error) {
	rows, err := q.Query(ctx, `/* pg_sage policy_own_documents v1 */
SELECT id, database_id, version, profile, doc, ratified_by, activated_at,
       COALESCE(set_name, '')
FROM sage.policy
WHERE database_id IS NOT NULL AND set_name IS NULL AND status = 'active'
  AND ($1::bigint IS NULL OR database_id = $1)`, only)
	if err != nil {
		return nil, fmt.Errorf("read per-database policies: %w", err)
	}
	defer rows.Close()
	out := map[int64]*Policy{}
	for rows.Next() {
		item, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan per-database policy: %w", err)
		}
		out[*item.Scope.DatabaseID] = &item
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate per-database policies: %w", err)
	}
	return out, nil
}

func readExceptions(
	ctx context.Context, q querier, only *int64,
) (map[int64]*Exception, error) {
	rows, err := q.Query(ctx, `/* pg_sage policy_exceptions v1 */
SELECT database_id, set_name, set_by, reason, set_at, expires_at
FROM sage.policy_exceptions WHERE $1::bigint IS NULL OR database_id = $1`, only)
	if err != nil {
		return nil, fmt.Errorf("read policy exceptions: %w", err)
	}
	defer rows.Close()
	out := map[int64]*Exception{}
	for rows.Next() {
		var e Exception
		if err := rows.Scan(&e.DatabaseID, &e.SetName, &e.SetBy, &e.Reason, &e.SetAt,
			&e.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan policy exception: %w", err)
		}
		out[e.DatabaseID] = &e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policy exceptions: %w", err)
	}
	return out, nil
}
