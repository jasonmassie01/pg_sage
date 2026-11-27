package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// vacuum-01: VACUUM of a partitioned parent recurses into every partition,
// a full-tree pass (with FREEZE: a WAL and full-page-image storm) that no
// producer may start unattended. Before the standing gate, a VACUUM
// finding's target is resolved as the statement names it (quoted parts
// verbatim, so mixed-case and dotted names resolve) and its relkind read.

// vacuumRelkindSQL reads the relkind of the relation a name resolves to;
// no row when it no longer exists. to_regclass takes no lock.
const vacuumRelkindSQL = `/* pg_sage executor */ SELECT c.relkind::text
  FROM pg_catalog.pg_class c WHERE c.oid = pg_catalog.to_regclass($1)`

// vacuumRelkindTimeout bounds the one catalog read per candidate.
const vacuumRelkindTimeout = 5 * time.Second

var errVacuumTargetGone = errors.New("relation no longer exists")

// guardVacuumTarget returns f, marked approval_required (on a copy) when
// its VACUUM targets a partitioned parent, names no single table, or the
// target's relkind could not be read (fail closed). stale is true when
// the target no longer exists: the finding is skipped, not queued.
func (e *Executor) guardVacuumTarget(
	ctx context.Context, f analyzer.Finding,
) (guarded analyzer.Finding, stale bool) {
	if !isVacuumStatement(f.RecommendedSQL) {
		return f, false
	}
	target, ok := singleVacuumTarget(f.RecommendedSQL)
	if !ok {
		return withApprovalReason(f, "the VACUUM does not name exactly one table, "+
			"so it may reach a partitioned parent: an operator must approve it"), false
	}
	relkind, err := e.vacuumTargetRelkind(ctx, target)
	switch {
	case errors.Is(err, errVacuumTargetGone):
		e.logFn("executor", "skipped %q: VACUUM target %s no longer exists "+
			"(stale finding)", f.Title, target)
		return f, true
	case err != nil:
		e.logFn("executor", "queueing %q for approval: relkind of VACUUM target %s "+
			"could not be read: %v", f.Title, target, err)
		return withApprovalReason(f, fmt.Sprintf("the relkind of %s could not be "+
			"read, so it may be a partitioned parent: an operator must approve it",
			target)), false
	case relkind == "p":
		return withApprovalReason(f, fmt.Sprintf("%s is a partitioned table: VACUUM "+
			"recurses into every partition, so an operator must approve it",
			target)), false
	}
	return f, false
}

// vacuumTargetRelkind reads the relkind of target (as the statement
// writes it), errVacuumTargetGone when it resolves to no relation.
func (e *Executor) vacuumTargetRelkind(ctx context.Context, target string) (string, error) {
	if e.pool == nil {
		return "", errors.New("no database connection")
	}
	ctx, cancel := context.WithTimeout(ctx, vacuumRelkindTimeout)
	defer cancel()
	var relkind string
	err := e.pool.QueryRow(ctx, vacuumRelkindSQL, target).Scan(&relkind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errVacuumTargetGone
	}
	if err != nil {
		return "", fmt.Errorf("read relkind of %s: %w", target, err)
	}
	return relkind, nil
}

// isVacuumStatement reports any VACUUM, FULL included.
func isVacuumStatement(sql string) bool {
	upper := strings.ToUpper(normalizeSQLText(sql))
	if !strings.HasPrefix(upper, "VACUUM") {
		return false
	}
	rest := upper[len("VACUUM"):]
	return rest == "" || rest[0] == ' ' || rest[0] == '(' || rest[0] == ';'
}

// singleVacuumTarget is the one relation a VACUUM names, as written (quoted
// parts verbatim), past its option list, bare option keywords, ONLY
// (PostgreSQL 18) and any column list; ok is false when it names none or
// several.
func singleVacuumTarget(sql string) (string, bool) {
	rest := strings.TrimSpace(normalizeSQLText(sql))
	rest = strings.TrimSpace(strings.TrimSuffix(rest, ";"))
	if !isVacuumStatement(rest) {
		return "", false
	}
	rest = strings.TrimSpace(rest[len("VACUUM"):])
	if strings.HasPrefix(rest, "(") {
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return "", false
		}
		rest = strings.TrimSpace(rest[end+1:])
	}
	for {
		word, after, _ := strings.Cut(rest, " ")
		switch strings.ToUpper(word) {
		case "FULL", "FREEZE", "VERBOSE", "ANALYZE", "ONLY":
			rest = after
			continue
		}
		break
	}
	relations := topLevelRelations(rest)
	if len(relations) != 1 {
		return "", false
	}
	return relations[0], true
}

// topLevelRelations splits a VACUUM relation list at its top-level commas,
// dropping column lists and unquoted spaces; quoted identifiers are kept
// verbatim.
func topLevelRelations(list string) []string {
	var out []string
	var cur strings.Builder
	quoted, depth := false, 0
	flush := func() {
		if name := cur.String(); name != "" {
			out = append(out, name)
		}
		cur.Reset()
	}
	for _, r := range list {
		switch {
		case r == '"':
			quoted = !quoted // a doubled quote toggles twice: still inside
		case quoted:
		case r == '(':
			depth++
			continue
		case r == ')':
			depth--
			continue
		case depth > 0 || r == ' ':
			continue
		case r == ',':
			flush()
			continue
		}
		if depth == 0 {
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
