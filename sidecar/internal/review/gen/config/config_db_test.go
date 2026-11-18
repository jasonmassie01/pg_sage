package config

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/gen/config"))
}

// liveSettings reads every pg_settings row as evid-02b's settings section
// states it (setting, unit and source as pg_settings prints them).
func liveSettings(t *testing.T) ([]workpack.SettingFact, string) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	var version string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT name, COALESCE(setting, ''), COALESCE(unit, ''),
		COALESCE(source, '') FROM pg_settings ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []workpack.SettingFact
	for rows.Next() {
		var f workpack.SettingFact
		if err := rows.Scan(&f.Name, &f.Value, &f.Unit, &f.Source); err != nil {
			t.Fatal(err)
		}
		f.ID, f.Changes = "setting/"+f.Name, []workpack.SettingChange{}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil || len(out) < 100 {
		t.Fatalf("read %d settings: %v", len(out), err)
	}
	return out, version
}

// Integration: a real server's settings, with temp files and requested
// checkpoints measured, give steps whose SQL the executor's validation and
// classifier accept as alter_system_guc, with every value in range; every
// non-default setting is listed.
func TestGenerateDB_RealSettings(t *testing.T) {
	settings, version := liveSettings(t)
	f := newFixture()
	f.settings, f.version = settings, version
	f.measures.TempBytesPerHour = 2 * gib
	f.measures.CheckpointsRequested, f.measures.CheckpointsTimed = 30.0, 1.0
	f.measures.WALBytesPerHour = 40 * gib
	cands, _ := generate(t, f)
	steps := 0
	for _, c := range cands {
		if c.ActionType != "alter_system_guc" {
			continue
		}
		steps++
		if got, _ := executor.ClassifySQL(c.ForwardSQL); got != "alter_system_guc" {
			t.Fatalf("%q classifies as %q", c.ForwardSQL, got)
		}
		for _, sql := range []string{c.ForwardSQL, c.RollbackSQL} {
			if err := executor.ValidateExecutorSQL(sql); err != nil {
				t.Fatalf("the executor refuses %q: %v", sql, err)
			}
		}
		name, value := emittedValue(t, c)
		if ok, why := pgconf.ValidateValue(name, value); !ok {
			t.Fatalf("%s=%s: %s", name, value, why)
		}
	}
	if steps == 0 {
		t.Fatalf("no alter_system_guc step from 2GiB/h of temp files and 30 requested "+
			"checkpoints: %+v", cands)
	}
	only(t, cands, KindMaxWALSize)
	nd, err := NonDefaults(f.pack(t))
	if err != nil || len(nd) == 0 {
		t.Fatalf("non-defaults %d (%v), want the server's configured settings", len(nd), err)
	}
	for _, s := range nd {
		if s.Source == "default" {
			t.Fatalf("a default setting %s was listed", s.Name)
		}
	}
}
