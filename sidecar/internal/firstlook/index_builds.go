package firstlook

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// indexBuildsSQL reads the index builds in progress in this database.
// Another role's build shows NULL relid without pg_read_all_stats.
const indexBuildsSQL = tag + `SELECT p.relid, p.index_relid, p.command, p.phase
FROM pg_catalog.pg_stat_progress_create_index p
WHERE p.datid = (SELECT d.oid FROM pg_catalog.pg_database d
                 WHERE d.datname = pg_catalog.current_database())
LIMIT $1`

// maxIndexBuilds bounds the progress read; one build per backend.
const maxIndexBuilds = 100

// uniqueKeysSQL names the key columns of the given indexes in key order,
// "" for an expression. indnullsnotdistinct exists from PostgreSQL 15, so
// it is read through to_jsonb and is false on older servers.
const uniqueKeysSQL = tag + `SELECT i.indexrelid,
  ARRAY(SELECT COALESCE(a.attname::text, '')
        FROM unnest(i.indkey::int2[]) WITH ORDINALITY k(attnum, ord)
        LEFT JOIN pg_catalog.pg_attribute a
          ON a.attrelid = i.indrelid AND a.attnum = k.attnum
        WHERE k.ord <= i.indnkeyatts
        ORDER BY k.ord) AS keys,
  COALESCE((pg_catalog.to_jsonb(i) ->> 'indnullsnotdistinct')::boolean, false) AS nnd
FROM pg_catalog.pg_index i
WHERE i.indexrelid = ANY($1::oid[])`

// inSavepoint runs fn in a savepoint of tx, so a failure inside cannot
// abort the step's transaction.
func inSavepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	if err := fn(sp); err != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return errors.Join(err, fmt.Errorf("roll back savepoint: %w", rbErr))
		}
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}

// readIndexBuilds reads the builds in progress. It never fails the step:
// a failed read, or builds hidden from the role, becomes Unseen.
func readIndexBuilds(ctx context.Context, tx pgx.Tx) IndexBuilds {
	var b IndexBuilds
	hidden := 0
	err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
		b, hidden = IndexBuilds{}, 0
		rows, err := sp.Query(ctx, indexBuildsSQL, maxIndexBuilds)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rel, index *uint32
			var command, phase *string
			if err := rows.Scan(&rel, &index, &command, &phase); err != nil {
				return err
			}
			if rel == nil {
				hidden++
				continue
			}
			bd := IndexBuild{TableOID: *rel}
			if index != nil {
				bd.IndexOID = *index
			}
			if command != nil {
				bd.Command = *command
			}
			if phase != nil {
				bd.Phase = *phase
			}
			b.Builds = append(b.Builds, bd)
		}
		return rows.Err()
	})
	if err != nil {
		return IndexBuilds{Unseen: "pg_stat_progress_create_index could not be read: " +
			err.Error()}
	}
	if hidden > 0 {
		b.Unseen = fmt.Sprintf("%d index builds by other roles are hidden from the "+
			"pg_sage role; GRANT pg_read_all_stats shows them", hidden)
	}
	return b
}

// readUniqueKeys names the key columns of the invalid unique indexes in
// idx, so the rule can suggest a duplicate-key query. It sends nothing
// when there are none.
func readUniqueKeys(ctx context.Context, tx pgx.Tx, idx []Index) error {
	pos := map[uint32]int{}
	var oids []uint32
	for i, x := range idx {
		if !x.Valid && x.Unique {
			pos[x.OID] = i
			oids = append(oids, x.OID)
		}
	}
	if len(oids) == 0 {
		return nil
	}
	err := inSavepoint(ctx, tx, func(sp pgx.Tx) error {
		rows, err := sp.Query(ctx, uniqueKeysSQL, oids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var oid uint32
			var keys []string
			var nnd bool
			if err := rows.Scan(&oid, &keys, &nnd); err != nil {
				return err
			}
			if i, ok := pos[oid]; ok {
				idx[i].KeyNames, idx[i].NullsNotDistinct = keys, nnd
			}
		}
		return rows.Err()
	})
	if err != nil {
		return fmt.Errorf("read unique index keys: %w", err)
	}
	return nil
}
