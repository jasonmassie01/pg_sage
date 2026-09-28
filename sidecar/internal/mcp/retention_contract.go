package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// retentionSpec is the owner's retention declaration (decision D5): the
// interval and the column whose age defines it. pg_sage never infers the
// column, so an interval without one is rejected.
type retentionSpec struct {
	Interval string
	Column   string
}

const retentionColumnRequired = "retention.column is required: name the timestamptz, " +
	"timestamp or date column whose age defines retention"

func parseRetention(raw json.RawMessage) (retentionSpec, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return retentionSpec{}, nil
	}
	var bare string
	if json.Unmarshal(raw, &bare) == nil {
		return retentionSpec{}, errors.New(
			`retention must be {"interval": ..., "column": ...}; ` + retentionColumnRequired)
	}
	var value struct {
		Interval string `json:"interval"`
		Column   string `json:"column"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return retentionSpec{}, fmt.Errorf("decode retention: %w", err)
	}
	spec := retentionSpec{
		Interval: strings.TrimSpace(value.Interval), Column: strings.TrimSpace(value.Column),
	}
	if spec.Interval == "" {
		return retentionSpec{}, errors.New("retention.interval is required")
	}
	if spec.Column == "" {
		return retentionSpec{}, errors.New(retentionColumnRequired)
	}
	return spec, nil
}

// retentionColumnSQL resolves a live, non-dropped column of a table and
// whether it is (part of) the partition key of a partitioned table.
const retentionColumnSQL = `SELECT tbl.relkind::text, typ.typname::text,
	COALESCE(att.attnum = ANY(pt.partattrs::int2[]), false)
FROM pg_class tbl
JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
JOIN pg_attribute att ON att.attrelid=tbl.oid AND att.attname=$3
	AND att.attnum>0 AND NOT att.attisdropped
JOIN pg_type typ ON typ.oid=att.atttypid
LEFT JOIN pg_partitioned_table pt ON pt.partrelid=tbl.oid
WHERE ns.nspname=$1 AND tbl.relname=$2`

// validateRetentionColumn rejects a declaration whose retention column does
// not exist, is dropped, or is not timestamptz, timestamp or date. A column
// that is not the partition key of a partitioned table is allowed, with a
// warning: each retention batch then scans every partition.
func (access *PostgresAccess) validateRetentionColumn(
	ctx context.Context, declaration TableContractDeclaration,
) ([]string, error) {
	if declaration.Retention == "" {
		return nil, nil
	}
	if declaration.RetentionColumn == "" {
		return nil, errors.New(retentionColumnRequired)
	}
	target := declaration.Schema + "." + declaration.Table
	var relkind, typeName string
	var partitionKey bool
	err := access.pool.QueryRow(ctx, retentionColumnSQL, declaration.Schema,
		declaration.Table, declaration.RetentionColumn).Scan(&relkind, &typeName, &partitionKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("retention column %q does not exist on %s",
			declaration.RetentionColumn, target)
	}
	if err != nil {
		return nil, fmt.Errorf("validate retention column: %w", err)
	}
	if relkind != "r" && relkind != "p" {
		return nil, fmt.Errorf("retention target %s is not a table", target)
	}
	switch typeName {
	case "timestamptz", "timestamp", "date":
	default:
		return nil, fmt.Errorf("retention column %q is %s; it must be timestamptz, "+
			"timestamp or date", declaration.RetentionColumn, typeName)
	}
	if relkind == "p" && !partitionKey {
		return []string{fmt.Sprintf("retention column %q is not the partition key of %s; "+
			"each retention batch scans every partition", declaration.RetentionColumn,
			target)}, nil
	}
	return nil, nil
}
