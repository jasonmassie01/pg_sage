package perfgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrGenericPlanUnsupported reports a server older than PostgreSQL 16,
// which cannot plan a statement with unbound parameters.
var ErrGenericPlanUnsupported = errors.New(
	"perfgate: EXPLAIN (GENERIC_PLAN) needs PostgreSQL 16+")

type planNode struct {
	NodeType string     `json:"Node Type"`
	Schema   string     `json:"Schema"`
	Relation string     `json:"Relation Name"`
	Plans    []planNode `json:"Plans"`
}

type planDoc struct {
	Plan *planNode `json:"Plan"`
}

// SeqScans returns the distinct sage relations a JSON plan scans
// sequentially, sorted.
func SeqScans(raw []byte) ([]PlanScan, error) {
	var docs []planDoc
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, fmt.Errorf("perfgate: parse plan: %w", err)
	}
	if len(docs) == 0 || docs[0].Plan == nil {
		return nil, errors.New("perfgate: plan document has no Plan")
	}
	seen := map[PlanScan]bool{}
	var walk func(n planNode)
	walk = func(n planNode) {
		if n.NodeType == "Seq Scan" && n.Schema == "sage" {
			seen[PlanScan{Schema: n.Schema, Relation: n.Relation}] = true
		}
		for _, child := range n.Plans {
			walk(child)
		}
	}
	walk(*docs[0].Plan)
	out := make([]PlanScan, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Relation < out[j].Relation })
	return out, nil
}

var (
	blockComment = regexp.MustCompile(`(?s)^/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`^--[^\n]*\n`)
	leadingVerb  = regexp.MustCompile(`(?i)^(select|with|insert|update|delete|merge)\b`)
	sageRef      = regexp.MustCompile(`(?i)(^|[^a-z0-9_])"?sage"?\.`)
)

// Explainable reports whether a statement is a single plannable DML or
// query statement (utility statements, multi-statement strings and
// EXPLAIN itself are not).
func Explainable(q string) bool {
	body := stripLeadingComments(q)
	if body == "" || !leadingVerb.MatchString(body) {
		return false
	}
	trimmed := strings.TrimRight(strings.TrimSpace(body), ";")
	return !strings.Contains(trimmed, ";")
}

func stripLeadingComments(q string) string {
	body := strings.TrimSpace(q)
	for {
		switch {
		case strings.HasPrefix(body, "/*"):
			loc := blockComment.FindStringIndex(body)
			if loc == nil {
				return ""
			}
			body = strings.TrimSpace(body[loc[1]:])
		case strings.HasPrefix(body, "--"):
			loc := lineComment.FindStringIndex(body)
			if loc == nil {
				return ""
			}
			body = strings.TrimSpace(body[loc[1]:])
		default:
			return body
		}
	}
}

var (
	intervalParam = regexp.MustCompile(`(?i)\binterval\s+(\$[0-9]+)`)
	extractParam  = regexp.MustCompile(`(?i)\bextract\(\s*\$[0-9]+\s+from\b`)
)

// repairNormalized undoes the pg_stat_statements normalizations that are
// not valid SQL on their own: a typed literal (interval '5 min' becomes
// interval $1) and an EXTRACT field (EXTRACT(epoch FROM ...)). The field is
// assumed to be epoch, the only one pg_sage extracts.
func repairNormalized(q string) string {
	q = intervalParam.ReplaceAllString(q, "${1}::interval")
	return extractParam.ReplaceAllString(q, "extract(epoch from")
}

// TouchesSage reports whether a statement names a sage.* relation.
func TouchesSage(q string) bool { return sageRef.MatchString(q) }

// ExplainStatements plans every explainable statement that touches the
// sage schema with EXPLAIN (GENERIC_PLAN): the plan PostgreSQL falls back
// to for a prepared statement, with no parameter values. Nothing runs.
// Statements go over the simple query protocol: their $n placeholders are
// the server's to plan, not parameters to bind.
func ExplainStatements(
	ctx context.Context, pool *pgxpool.Pool, stmts []Statement,
) ([]PlanResult, error) {
	var version int
	if err := pool.QueryRow(ctx, "/* "+HarnessTag+" */ "+
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		return nil, fmt.Errorf("perfgate: server version: %w", err)
	}
	if version < 160000 {
		return nil, ErrGenericPlanUnsupported
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("perfgate: acquire connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET statement_timeout = '5s'"); err != nil {
		return nil, fmt.Errorf("perfgate: bound explain time: %w", err)
	}
	var out []PlanResult
	for _, s := range stmts {
		if !Explainable(s.Query) || !TouchesSage(s.Query) {
			continue
		}
		out = append(out, explainOne(ctx, conn.Conn().PgConn(), s))
	}
	return out, nil
}

func explainOne(ctx context.Context, pc *pgconn.PgConn, s Statement) PlanResult {
	res := PlanResult{Statement: s}
	results, err := pc.Exec(ctx, "EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON) "+
		repairNormalized(s.Query)).
		ReadAll()
	if err == nil && (len(results) != 1 || len(results[0].Rows) != 1) {
		err = errors.New("EXPLAIN returned no plan")
	}
	if err != nil {
		res.Err = err.Error()
		return res
	}
	scans, err := SeqScans(results[0].Rows[0][0])
	if err != nil {
		res.Err = err.Error()
		return res
	}
	res.SeqScans = scans
	return res
}
