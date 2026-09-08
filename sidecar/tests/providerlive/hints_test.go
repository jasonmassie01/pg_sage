//go:build providerlive

package providerlive

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/fleet"
)

func checkHints(t *testing.T, f fixture) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := f.pool.Acquire(ctx)
	checkError(t, "acquire dedicated hint session", err)
	defer conn.Release()
	ensureHintHook(t, ctx, f, conn)
	for hint, expected := range map[string]string{
		"SeqScan(i)": "Seq Scan", "IndexScan(i items_category_idx)": "Index Scan",
	} {
		var raw []byte
		sql := "EXPLAIN (FORMAT JSON) SELECT /*+ " + hint + " */ id FROM " +
			f.table("items") + " AS i WHERE category=42"
		checkError(t, "execute hinted plan", conn.QueryRow(ctx, sql).Scan(&raw))
		var plans []struct {
			Plan struct {
				NodeType string `json:"Node Type"`
			} `json:"Plan"`
		}
		checkError(t, "decode hinted plan", json.Unmarshal(raw, &plans))
		if len(plans) != 1 || plans[0].Plan.NodeType != expected {
			t.Fatalf("hint %s did not produce required %s", hint, expected)
		}
	}
}

func ensureHintHook(t *testing.T, ctx context.Context, f fixture, conn *pgxpool.Conn) {
	t.Helper()
	var installed bool
	checkError(t, "discover pg_hint_plan", conn.QueryRow(ctx, `SELECT EXISTS
		(SELECT 1 FROM pg_catalog.pg_extension WHERE extname='pg_hint_plan')`).Scan(&installed))
	if !installed {
		t.Skip("pg_hint_plan not installed; hint execution NOT verified")
	}
	var loaded bool
	checkError(t, "discover hint hook", conn.QueryRow(ctx, `SELECT EXISTS
		(SELECT 1 FROM pg_settings WHERE name='pg_hint_plan.enable_hint')`).Scan(&loaded))
	if !loaded {
		_, err := conn.Exec(ctx, "LOAD 'pg_hint_plan'")
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" {
			verifyInactiveHintCapability(t, ctx, f, conn)
			t.Skip("hint execution NOT verified: module inactive, LOAD42501 and no preload SET privilege")
		}
		checkError(t, "load session hint hook", err)
	}
}

func verifyInactiveHintCapability(t *testing.T, ctx context.Context,
	f fixture, conn *pgxpool.Conn,
) {
	t.Helper()
	var mayPreload bool
	checkError(t, "verify preload restriction", conn.QueryRow(ctx,
		"SELECT has_parameter_privilege(current_user,'session_preload_libraries','SET')",
	).Scan(&mayPreload))
	if mayPreload {
		t.Fatal("hint activation remains possible; do not skip execution probe")
	}
	caps := fleet.CollectProviderCapabilities(ctx, f.pool, nil,
		f.provider, "manual", false, time.Now())
	if caps.Extensions["pg_hint_plan"] != "installed_not_loaded" {
		t.Fatal("product misreported installed but inactive hint module")
	}
}
