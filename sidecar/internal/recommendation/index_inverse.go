package recommendation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// The inverse of CREATE INDEX <name> is the drop of that same index.
// Releases before v1.8.0 stored the LLM's drop_ddl as a finding's
// rollback_sql and refreshed recommended_sql without it (C04, fixed in
// 6e9d9f92), so an open legacy finding can pair a create with a drop of
// another index; MigrateLegacy then copied the pair into a revision
// (lifeos 1.8.3, finding 18007: creates idx_memories_active_query_opt,
// rollback drops idx_memories_active_partial).

const dropIdent = `(?:"(?:[^"]|"")+"|[A-Za-z_][A-Za-z0-9_$]*)`

// dropIndexPattern is one DROP INDEX of one (optionally qualified) index,
// never CASCADE.
var dropIndexPattern = regexp.MustCompile(`(?is)^\s*DROP\s+INDEX\s+` +
	`(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?(` + dropIdent + `(?:\s*\.\s*` + dropIdent +
	`)?)\s*(?:RESTRICT\s*)?;?\s*$`)

// InverseDropsCreatedIndex reports whether inverse drops exactly the
// index the CREATE INDEX forward creates (same name, and same schema when
// both name one). An unnamed or unparseable create matches nothing.
func InverseDropsCreatedIndex(forward, inverse string) bool {
	spec, err := optimizer.ParseIndexDDL(forward)
	if err != nil || spec.Name == "" {
		return false
	}
	match := dropIndexPattern.FindStringSubmatch(inverse)
	if len(match) != 2 {
		return false
	}
	parts := splitDropTarget(match[1])
	if parts[len(parts)-1] != spec.Name {
		return false
	}
	return len(parts) == 1 || spec.TableSchema == "" || parts[0] == spec.TableSchema
}

// DerivedIndexInverse is the deterministic inverse of a named CREATE
// INDEX: DROP INDEX CONCURRENTLY IF EXISTS of the created index, schema
// qualified when the create names the table's schema.
func DerivedIndexInverse(forward string) (string, bool) {
	spec, err := optimizer.ParseIndexDDL(forward)
	if err != nil || spec.Name == "" {
		return "", false
	}
	target := sanitize.QuoteIdentifier(spec.Name)
	if spec.TableSchema != "" {
		target = sanitize.QuoteQualifiedName(spec.TableSchema, spec.Name)
	}
	return "DROP INDEX CONCURRENTLY IF EXISTS " + target, true
}

// repairedInverse returns the derived inverse when inverse is a non-empty
// rollback that does not drop the index forward creates.
func repairedInverse(forward, inverse string) (string, bool) {
	if strings.TrimSpace(inverse) == "" || InverseDropsCreatedIndex(forward, inverse) {
		return "", false
	}
	return DerivedIndexInverse(forward)
}

var dropIdentPattern = regexp.MustCompile(dropIdent)

// splitDropTarget splits a matched [schema.]name into normalized parts:
// quoted parts unescaped, bare parts case-folded.
func splitDropTarget(target string) []string {
	parts := dropIdentPattern.FindAllString(target, -1)
	for i, part := range parts {
		if strings.HasPrefix(part, `"`) {
			parts[i] = strings.ReplaceAll(part[1:len(part)-1], `""`, `"`)
		} else {
			parts[i] = strings.ToLower(part)
		}
	}
	return parts
}

// repairFindingInverses rewrites the stale CREATE INDEX rollback of open
// findings (the source MigrateLegacy and the operator surfaces read).
func (s *Store) repairFindingInverses(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT id, recommended_sql, rollback_sql
		FROM sage.findings
		WHERE status = 'open' AND COALESCE(rollback_sql, '') <> ''
		  AND recommended_sql ~* '^\s*CREATE\s+(UNIQUE\s+)?INDEX'
		ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("list open index findings: %w", err)
	}
	type row struct {
		id                int64
		forward, rollback string
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.id, &x.forward, &x.rollback)
	})
	if err != nil {
		return 0, fmt.Errorf("read open index findings: %w", err)
	}
	repaired := 0
	for _, f := range found {
		derived, stale := repairedInverse(f.forward, f.rollback)
		if !stale {
			continue
		}
		tag, err := s.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.findings
			SET rollback_sql = $2 WHERE id = $1 AND rollback_sql = $3`,
			f.id, derived, f.rollback)
		if err != nil {
			return repaired, fmt.Errorf("repair rollback of finding %d: %w", f.id, err)
		}
		repaired += int(tag.RowsAffected())
	}
	return repaired, nil
}

// staleRevisionSQL selects unapproved live CREATE INDEX recommendations
// with their current revision's content.
const staleRevisionSQL = `/* pg_sage */ SELECT r.category, r.target, v.forward_sql,
	COALESCE(v.inverse_sql, ''), v.evidence, v.policy_version, COALESCE(v.title, ''),
	COALESCE(v.severity, ''), COALESCE(v.object_type, ''), COALESCE(v.action_risk, ''),
	COALESCE(v.recommendation, '')
	FROM sage.recommendation r
	JOIN sage.recommendation_revision v
	  ON v.recommendation_id = r.id AND v.revision = r.revision
	WHERE r.database_name = $1 AND r.action_type = 'create_index'
	  AND r.state IN ` + revisableStatesSQL + ` AND r.approved_hash IS NULL
	ORDER BY r.id`

// repairRevisionInverses appends a corrected revision to every unapproved
// live recommendation whose CREATE INDEX inverse drops another index. An
// approved one is left as approved — an approval pins exactly the content
// approved (C04) — and the executor refuses to run it.
func (s *Store) repairRevisionInverses(ctx context.Context, database string) (int, error) {
	rows, err := s.pool.Query(ctx, staleRevisionSQL, database)
	if err != nil {
		return 0, fmt.Errorf("list index recommendations: %w", err)
	}
	proposals, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Proposal, error) {
		return scanRevisionProposal(r, database)
	})
	if err != nil {
		return 0, fmt.Errorf("read index recommendations: %w", err)
	}
	repaired := 0
	for _, p := range proposals {
		derived, stale := repairedInverse(p.ForwardSQL, p.InverseSQL)
		if !stale {
			continue
		}
		p.InverseSQL = derived
		err := s.withTx(ctx, func(tx pgx.Tx) error {
			_, err := proposeTx(ctx, tx, p, SourceMigrated)
			return err
		})
		if err != nil {
			return repaired, fmt.Errorf("repair inverse of %s/%s: %w", p.Category, p.Target, err)
		}
		repaired++
	}
	return repaired, nil
}

func scanRevisionProposal(r pgx.Row, database string) (Proposal, error) {
	p := Proposal{DatabaseName: database}
	var evidence []byte
	err := r.Scan(&p.Category, &p.Target, &p.ForwardSQL, &p.InverseSQL, &evidence,
		&p.PolicyVersion, &p.Title, &p.Severity, &p.ObjectType, &p.ActionRisk,
		&p.Recommendation)
	if err != nil {
		return p, err
	}
	if len(evidence) > 0 {
		if err := json.Unmarshal(evidence, &p.Evidence); err != nil {
			return p, fmt.Errorf("decode revision evidence: %w", err)
		}
	}
	return p, nil
}
