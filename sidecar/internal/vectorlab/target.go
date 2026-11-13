package vectorlab

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// validateTarget checks the table contract and returns the embedding type
// (vector or halfvec) as the catalog records it.
func (s *postgresSource) validateTarget(ctx context.Context) (string, error) {
	name := pgx.Identifier{s.manifest.Schema, s.manifest.Table}.Sanitize()
	var oid uint32
	if err := s.tx.QueryRow(ctx, "SELECT $1::regclass::oid", name).Scan(&oid); err != nil {
		return "", safeError("resolve target table", err)
	}
	var ordinary, identity bool
	var vectorType *string
	err := s.tx.QueryRow(ctx, `SELECT c.relkind='r',
		EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_index i ON i.indrelid=a.attrelid
		WHERE a.attrelid=c.oid AND a.attname=$2 AND a.attnotnull AND NOT a.attisdropped
		AND i.indisunique AND i.indisvalid AND i.indisready AND i.indnkeyatts=1
		AND i.indpred IS NULL AND i.indexprs IS NULL AND i.indkey[0]=a.attnum),
		(SELECT t.typname::text FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_type t ON t.oid=a.atttypid
		JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace
		WHERE a.attrelid=c.oid AND a.attname=$3 AND NOT a.attisdropped
		AND t.typname IN ('vector','halfvec') AND n.nspname=$4)
		FROM pg_catalog.pg_class c WHERE c.oid=$1`, oid, s.manifest.IDColumn,
		s.manifest.VectorColumn, s.extensionSchema).Scan(&ordinary, &identity, &vectorType)
	if err != nil {
		return "", safeError("validate target table contract", err)
	}
	if !ordinary {
		return "", errors.New("target must be an ordinary table, not a view or partition root")
	}
	if !identity {
		return "", errors.New("id_column requires a valid single-column unique non-null key")
	}
	if vectorType == nil {
		return "", errors.New("vector_column must be a pgvector vector or halfvec column")
	}
	return *vectorType, checkTargetDimensions(*vectorType, len(s.manifest.Queries[0].Vector))
}
