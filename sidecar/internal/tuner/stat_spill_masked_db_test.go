package tuner

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// tuning-03 review: the server-wide work_mem baseline of a role without
// settings of its own. ALTER ROLE ALL SET is a tier of its own, and when
// pg_sage's own session masks the server default (a role-level setting of
// pg_sage's login role, or a client option), that default cannot be read,
// so such a role has no baseline and nothing is sized for it (fail closed),
// rather than guessing boot_val and hinting below its real work_mem.

// tunerPoolAs opens a pool on the test database as user (password "" keeps
// the DSN's) with extra startup options.
func tunerPoolAs(t *testing.T, user, password string,
	params map[string]string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.MaxConns = 1
	if user != "" {
		cfg.ConnConfig.User, cfg.ConnConfig.Password = user, password
	}
	for k, v := range params {
		cfg.ConnConfig.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect as %q: %v", user, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestStatSpillDB_AllRolesSettingIsTheBaseline(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	plain := spillRole(t, pool, "tuning03_allroles")
	execAll(t, pool, "ALTER ROLE ALL SET work_mem = '48MB'")
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "ALTER ROLE ALL RESET work_mem") })
	// pg_sage's own role has a setting, so its reset_val says nothing about
	// the ALL-roles tier: the tier itself must be read.
	spillRole(t, pool, "tuning03_sage_all")
	execAll(t, pool, "ALTER ROLE tuning03_sage_all PASSWORD 'tuning03-test'",
		"ALTER ROLE tuning03_sage_all SET work_mem = '2MB'")
	tu := New(tunerPoolAs(t, "tuning03_sage_all", "tuning03-test", nil),
		TunerConfig{WorkMemMaxMB: 512}, nil, noopLogFn)
	s, err := tu.readSpillSettings(ctx, []int64{plain})
	if err != nil {
		t.Fatalf("read spill settings: %v", err)
	}
	if got, ok := s.WorkMemKB[plain]; !ok || got != 48*1024 {
		t.Fatalf("ALTER ROLE ALL SET 48MB: baseline = %d kB (known %v), want 49152", got, ok)
	}
}

func TestStatSpillDB_MaskedServerDefaultFailsClosed(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	plain := spillRole(t, pool, "tuning03_masked_plain")
	app := spillRole(t, pool, "tuning03_masked_app")
	spillRole(t, pool, "tuning03_sage_role")
	execAll(t, pool, "ALTER ROLE tuning03_masked_app SET work_mem = '64MB'",
		"ALTER ROLE tuning03_sage_role PASSWORD 'tuning03-test'",
		"ALTER ROLE tuning03_sage_role SET work_mem = '2MB'")
	cases := map[string]*pgxpool.Pool{
		"pg_sage role setting": tunerPoolAs(t, "tuning03_sage_role", "tuning03-test", nil),
		"client option":        tunerPoolAs(t, "", "", map[string]string{"work_mem": "2MB"}),
	}
	for name, p := range cases {
		tu := New(p, TunerConfig{WorkMemMaxMB: 512}, nil, noopLogFn)
		s, err := tu.readSpillSettings(ctx, []int64{plain, app})
		if err != nil {
			t.Fatalf("%s: read spill settings: %v (a masked default is no error)", name, err)
		}
		if got, ok := s.WorkMemKB[plain]; ok {
			t.Fatalf("%s: role without settings got baseline %d kB, want none (the "+
				"server default is masked by pg_sage's own session)", name, got)
		}
		if s.WorkMemKB[app] != 64*1024 {
			t.Fatalf("%s: role setting = %d kB, want 65536", name, s.WorkMemKB[app])
		}
		if s.BlockSize < 1024 {
			t.Fatalf("%s: block_size = %d", name, s.BlockSize)
		}
		// Nothing is sized for the role without a baseline.
		tu.spill.read = func(context.Context, []int64) (spillSettings, error) { return s, nil }
		tu.observeSpills(ctx, []candidate{spillCand(91, plain, 10, 0)})
		c := spillCand(91, plain, 1_010, 1_000*16*1024*1024/s.BlockSize)
		tu.observeSpills(ctx, []candidate{c})
		if got := CombineHints(tu.prescribeAll(tu.gatherSymptoms(ctx, c))); got != "" {
			t.Fatalf("%s: hint = %q for a role whose work_mem is unknown", name, got)
		}
	}
}
