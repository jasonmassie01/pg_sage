package schema

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// config-04 review: the sage.config key index is trusted only when it is
// valid and unique, and a second boot takes no lock a writer would wait on.
// ACCESS SHARE (the pg_dump test) conflicts only with ACCESS EXCLUSIVE, and
// pg_locks sampling can miss a short SHARE lock; ROW EXCLUSIVE (any writer)
// conflicts with every lock CREATE INDEX, CREATE TRIGGER or ALTER TABLE
// takes, so a second boot that finishes under it took none of them.

// Happy path, the busy-writer case: a second boot while another session
// holds ROW EXCLUSIVE on every sage table finishes without a lock wait.
func TestBootstrap_SecondBootUnderRowExclusiveTakesNoShareLock(t *testing.T) {
	pool := freshSchemaPool(t, "schema_boot_rowx")
	holdLocks(t, pool, "ROW EXCLUSIVE", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	err := Bootstrap(ctx, pool)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("second boot under ROW EXCLUSIVE: %v", err)
	}
	if took >= migrationLockTimeout {
		t.Fatalf("second boot took %s, want under %s (no lock wait)", took,
			migrationLockTimeout)
	}
}

// Invalid state: a non-unique index named idx_config_key_db breaks every
// ON CONFLICT upsert on sage.config just as a missing one does, so
// readiness reports it.
func TestCheckReady_NonUniqueConfigKeyIndexIsNotReady(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ready_nonunique")
	ctx := context.Background()
	if err := CheckReady(ctx, pool); err != nil {
		t.Fatalf("CheckReady on a fresh schema: %v", err)
	}
	execAll(t, pool, "DROP INDEX sage.idx_config_key_db",
		"CREATE INDEX idx_config_key_db ON sage.config (key, COALESCE(database_id, 0))")
	err := CheckReady(ctx, pool)
	if !errors.Is(err, ErrSchemaNotReady) ||
		!strings.Contains(err.Error(), "sage.idx_config_key_db") {
		t.Fatalf("CheckReady = %v, want ErrSchemaNotReady naming sage.idx_config_key_db", err)
	}
}

// State transitions of the sage.config keys: an old install with the
// single-column primary key gets the composite index, then loses the
// primary key; a broken composite index fails the boot and keeps the
// primary key, so the table is never left without a unique key.
func TestBootstrap_ConfigPrimaryKeyIsDroppedOnlyBehindAGoodIndex(t *testing.T) {
	cases := []struct {
		name   string
		setup  []string
		wantPK bool
		errHas string
	}{
		{name: "index missing", setup: []string{"DROP INDEX sage.idx_config_key_db"}},
		{name: "index invalid", setup: []string{`UPDATE pg_index SET indisvalid = false
			WHERE indexrelid = 'sage.idx_config_key_db'::regclass`},
			wantPK: true, errHas: "idx_config_key_db is not a valid unique index"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := freshSchemaPool(t, "schema_boot_cfgpk")
			ctx := context.Background()
			execAll(t, pool, append([]string{
				"ALTER TABLE sage.config ADD CONSTRAINT config_pkey PRIMARY KEY (key)",
			}, tc.setup...)...)
			err := Bootstrap(ctx, pool)
			if tc.errHas == "" && err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			if tc.errHas != "" && (err == nil || !strings.Contains(err.Error(), tc.errHas)) {
				t.Fatalf("Bootstrap: err = %v, want it to contain %q", err, tc.errHas)
			}
			st, err := readConfigKeyIndexState(ctx, pool)
			if err != nil {
				t.Fatal(err)
			}
			if st.hasPK != tc.wantPK {
				t.Fatalf("config_pkey present = %t, want %t", st.hasPK, tc.wantPK)
			}
			if tc.errHas == "" && (!st.exists || !st.valid || !st.unique) {
				t.Fatalf("key index after boot = %+v, want valid and unique", st)
			}
		})
	}
}

func execAll(t *testing.T, pool *pgxpool.Pool, stmts ...string) {
	t.Helper()
	for _, stmt := range stmts {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}
