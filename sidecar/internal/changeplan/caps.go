package changeplan

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Plan caps (plan-02b): a plan concentrates change, so the standing
// policy's plans block bounds its sum. A plan over a cap is refused when
// it is proposed and when it is approved (CheckPlanCaps); before each step
// the engine checks the caps against every running plan of the database
// and the live sizes, and a step that would cross one, or start inside a
// peak window, waits (engine_caps.go).

// The caps' names (a waiting step's reason starts with one).
const (
	CapIndexesPerTable   = "max_new_indexes_per_table"
	CapIndexBytesPct     = "max_new_index_bytes_pct"
	CapRestartBoundSteps = "max_restart_bound_steps"
)

// Why a step waits besides a cap.
const (
	ReasonPeakWindow      = "peak_window"
	ReasonSizeUnavailable = "size_unavailable"
	ReasonCapsUnavailable = "caps_unavailable"
	ReasonCapWaitExpired  = "cap_wait_expired"
)

// ErrOverCap refuses a plan over a plan cap.
var ErrOverCap = errors.New("changeplan: the plan is over a plan cap")

// CapError names the cap a plan crosses, its limit, the plan's number and
// the object it concerns (a table; empty for a plan-wide cap).
type CapError struct {
	Cap    string
	Limit  int64
	Got    int64
	Object string
}

func (e *CapError) Error() string {
	on := ""
	if e.Object != "" {
		on = " on " + e.Object
	}
	return fmt.Sprintf("%s: %s is %d%s, the cap is %d", ErrOverCap, e.Cap, e.Got, on,
		e.Limit)
}

func (e *CapError) Unwrap() error { return ErrOverCap }

// CapsSource reads the plan caps in force (the standing policy's plans
// block). An error refuses proposals and makes steps wait (fail closed).
type CapsSource func(context.Context) (policy.PlanCaps, error)

// noPolicyCaps is the source without a policy: the defaults.
func noPolicyCaps(context.Context) (policy.PlanCaps, error) { return policy.PlanCaps{}, nil }

// CheckPlanCaps checks plan p against caps: at most MaxNewIndexesPerTable
// index steps on one table and at most one restart-bound step (a setting
// of unknown context counts as one). The byte cap needs the built sizes,
// so the engine checks it before each step.
func CheckPlanCaps(p Plan, caps policy.PlanCaps, settings SettingContexts) error {
	caps = caps.Resolved()
	perTable := map[string]int64{}
	var restart int64
	for _, s := range p.Steps {
		if _, table, ok := newIndexOf(s); ok {
			perTable[table]++
		}
		if why := settingClass(s, settings); s.Kind == KindAction && why != "" {
			restart++
		}
	}
	tables := make([]string, 0, len(perTable))
	for t := range perTable {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		if perTable[t] > caps.MaxNewIndexesPerTable {
			return &CapError{Cap: CapIndexesPerTable, Limit: caps.MaxNewIndexesPerTable,
				Got: perTable[t], Object: t}
		}
	}
	if restart > caps.MaxRestartBoundSteps {
		return &CapError{Cap: CapRestartBoundSteps, Limit: caps.MaxRestartBoundSteps,
			Got: restart}
	}
	return nil
}

// checkCapsIn reads the caps and the settings' contexts and checks p (the
// store's proposal and approval check).
func checkCapsIn(ctx context.Context, q querier, src CapsSource, p Plan) error {
	caps, err := src(ctx)
	if err != nil {
		return fmt.Errorf("read the plan caps: %w", err)
	}
	settings, err := readSettingContexts(ctx, q, p.Steps)
	if err != nil {
		return err
	}
	return CheckPlanCaps(p, caps, settings)
}

const identPart = `(?:"[^"]+"|[a-z_][a-z0-9_$]*)`

// reNewIndex reads CREATE [UNIQUE] INDEX [CONCURRENTLY] [IF NOT EXISTS]
// [name] ON [ONLY] table.
var reNewIndex = regexp.MustCompile(`(?is)^\s*CREATE\s+(?:UNIQUE\s+)?INDEX\s+` +
	`(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(` + identPart + `(?:\.` + identPart +
	`)?)?\s*\bON\s+(?:ONLY\s+)?(` + identPart + `(?:\.` + identPart + `)?)`)

// newIndexOf is the index action step s builds and its table, both
// schema-qualified and lower case outside quotes (an unqualified table is
// taken as public; an index lives in its table's schema). index is empty
// for an unnamed index; ok is false when s builds no index.
func newIndexOf(s Step) (index, table string, ok bool) {
	if s.Kind != KindAction {
		return "", "", false
	}
	m := reNewIndex.FindStringSubmatch(s.ProposedSQL)
	if m == nil {
		return "", "", false
	}
	table = qualify(m[2], "public")
	if m[1] != "" {
		index = qualify(m[1], splitIdent(table)[0])
	}
	return index, table, true
}

// qualify lower-cases name's unquoted parts and prefixes schema when name
// has none.
func qualify(name, schema string) string {
	parts := splitIdent(name)
	for i, p := range parts {
		if !strings.HasPrefix(p, `"`) {
			parts[i] = strings.ToLower(p)
		}
	}
	if len(parts) == 1 {
		return schema + "." + parts[0]
	}
	return strings.Join(parts, ".")
}

// splitIdent splits a possibly qualified identifier on the dot outside
// quotes.
func splitIdent(name string) []string {
	inQuote := false
	for i, r := range name {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '.' && !inQuote:
			return []string{name[:i], name[i+1:]}
		}
	}
	return []string{name}
}

// capStep is a started step of a running plan, as the caps count it.
type capStep struct {
	planID int64
	stepID int64
	sql    string
}

// indexCountWait is why index step s must wait for the per-table cap
// ("" when it may start): the steps of every running plan that built (or
// are building) an index on its table, plus s, may not exceed the cap.
func indexCountWait(s Step, others []capStep, caps policy.PlanCaps) string {
	_, table, ok := newIndexOf(s)
	if !ok {
		return ""
	}
	n := int64(1)
	for _, o := range others {
		if _, t, ok := newIndexOf(Step{Kind: KindAction, ProposedSQL: o.sql}); ok &&
			t == table {
			n++
		}
	}
	limit := caps.Resolved().MaxNewIndexesPerTable
	if n <= limit {
		return ""
	}
	return fmt.Sprintf("%s: %s would have %d new indexes from running plans, the cap is %d",
		CapIndexesPerTable, table, n, limit)
}

// indexBytesWait is why the next index step must wait for the byte cap
// ("" when it may start): the indexes running plans built hold built
// bytes of a database of dbBytes. The next index's own size is unknown
// until it is built, so the cap bounds what is built when it starts.
func indexBytesWait(built, dbBytes int64, caps policy.PlanCaps) string {
	if caps.IndexBytesWithin(built, dbBytes) {
		return ""
	}
	return fmt.Sprintf("%s: running plans built %d bytes of new indexes, the cap is %d "+
		"bytes (%d%% of the database's %d bytes)", CapIndexBytesPct, built,
		caps.IndexBytesCap(dbBytes), caps.Resolved().MaxNewIndexBytesPct, dbBytes)
}

// SizeReader reads the live sizes the byte cap needs.
type SizeReader interface {
	// DatabaseBytes is the monitored database's size.
	DatabaseBytes(ctx context.Context) (int64, error)
	// IndexBytes is the summed size of the named indexes that exist
	// (pg_relation_size); a name that does not resolve counts 0.
	IndexBytes(ctx context.Context, indexes []string) (int64, error)
}

// DBSizes reads sizes from the monitored database (read-only, bounded by
// statement_timeout).
type DBSizes struct{ pool *pgxpool.Pool }

// NewDBSizes reads sizes on pool.
func NewDBSizes(pool *pgxpool.Pool) DBSizes { return DBSizes{pool: pool} }

// DatabaseBytes is pg_database_size of the current database.
func (z DBSizes) DatabaseBytes(ctx context.Context) (int64, error) {
	var n int64
	err := (&Store{pool: z.pool}).inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `/* pg_sage */ SELECT
			pg_database_size(current_database())`).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("read the database size: %w", err)
	}
	return n, nil
}

// IndexBytes sums pg_relation_size of the indexes that exist.
func (z DBSizes) IndexBytes(ctx context.Context, indexes []string) (int64, error) {
	if len(indexes) == 0 {
		return 0, nil
	}
	var n int64
	err := (&Store{pool: z.pool}).inTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `/* pg_sage */ SELECT
			COALESCE(sum(pg_relation_size(to_regclass(n))), 0)::bigint
			FROM unnest($1::text[]) AS n`, indexes).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("read the size of the plans' indexes: %w", err)
	}
	return n, nil
}

// capsSource is the store's caps source (the defaults for a zero Store).
func (s *Store) capsSource() CapsSource {
	if s.caps == nil {
		return noPolicyCaps
	}
	return s.caps
}
