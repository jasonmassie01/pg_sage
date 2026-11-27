package rca

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Bounds for the catalog reads that prove Tier 2 SQL read-only. They run
// on the monitored database, so they are read-only and short.
const (
	tier2ProofTimeout   = 5 * time.Second
	tier2ProofStatement = "2s"
	tier2ProofLock      = "500ms"
)

// tier2ProofOptions is the proof model SQL must pass before an incident
// shows it: no SECURITY DEFINER functions, and catalog views are trusted
// as PostgreSQL defines them.
var tier2ProofOptions = explain.ReadProofOptions{
	DenySecurityDefiner:  true,
	AllowCatalogRelation: func(string, string) bool { return true },
}

// errNoCatalog is the catalog error when the engine has no store yet.
var errNoCatalog = errors.New("no catalog connection (incident store not bound)")

// vetTier2SQL proves inc.RecommendedSQL read-only on the store inside a
// bounded READ ONLY transaction, or withholds it (security-05). A nil
// store withholds every statement.
func vetTier2SQL(ctx context.Context, store *pgxpool.Pool, inc *Incident,
	logFn func(string, string, ...any)) {
	if inc.RecommendedSQL == "" {
		return
	}
	if store == nil {
		vetModelSQL(ctx, unavailableCatalog{}, nil, inc, logFn)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, tier2ProofTimeout)
	defer cancel()
	tx, err := beginProofTx(pctx, store)
	if err != nil {
		logFn("warn", "rca: tier2 model SQL withheld: open catalog transaction: %v", err)
		withholdSQL(inc, "it could not be checked against the catalog")
		return
	}
	defer func() {
		if err := tx.Rollback(context.WithoutCancel(pctx)); err != nil &&
			!errors.Is(err, pgx.ErrTxClosed) {
			logFn("warn", "rca: tier2 proof rollback: %v", err)
		}
	}()
	vetModelSQL(pctx, tx, nil, inc, logFn)
}

// beginProofTx opens the proof's bounded READ ONLY transaction on the
// application's search_path (apppath.Pin), the path a person runs the SQL
// on: the store's own sessions are pinned to pg_catalog, pg_temp, where a
// public function that wins resolution for that person would go unseen
// and an application table the SQL names without a schema would not
// resolve (security-01c-r).
func beginProofTx(ctx context.Context, store *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := store.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	for _, stmt := range []string{
		"SET LOCAL statement_timeout = '" + tier2ProofStatement + "'",
		"SET LOCAL lock_timeout = '" + tier2ProofLock + "'",
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	if err := apppath.Pin(ctx, tx); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return tx, nil
}

// vetModelSQL keeps inc.RecommendedSQL only when explain.ProveRead proves
// it can invoke nothing but side-effect-free objects. A refusal, a failed
// catalog read or a build without the parser withholds it: the SQL is
// cleared, a chain note says why and a WARN is logged. Incident creation
// is unchanged. inspect is nil for the libpg_query parser.
func vetModelSQL(ctx context.Context, q explain.CatalogQuerier,
	inspect func(string) (sqlast.ReadQuery, error), inc *Incident,
	logFn func(string, string, ...any)) {
	if inc.RecommendedSQL == "" {
		return
	}
	refusal, err := explain.ProveRead(ctx, q, inc.RecommendedSQL, inspect, tier2ProofOptions)
	if err != nil {
		logFn("warn", "rca: tier2 model SQL withheld: catalog read failed: %v", err)
		withholdSQL(inc, "it could not be checked against the catalog")
		return
	}
	if refusal == "" {
		return
	}
	logFn("warn", "rca: tier2 model SQL withheld: it %s", refusal)
	withholdSQL(inc, "it "+refusal)
}

// withholdSQL clears the model SQL and appends the reason to the chain.
// The note does not repeat the SQL.
func withholdSQL(inc *Incident, reason string) {
	inc.RecommendedSQL = ""
	inc.CausalChain = append(inc.CausalChain, ChainLink{
		Order:       len(inc.CausalChain) + 1,
		Description: "model SQL withheld: " + reason,
	})
}

// unavailableCatalog fails every catalog read, so a proof that needs the
// catalog fails closed.
type unavailableCatalog struct{}

func (unavailableCatalog) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errNoCatalog
}

func (unavailableCatalog) QueryRow(context.Context, string, ...any) pgx.Row {
	return failedRow{}
}

type failedRow struct{}

func (failedRow) Scan(...any) error { return errNoCatalog }
