package upgradecheck

import (
	"sort"
	"strings"
	"testing"
)

// A primary key does not make a table replicable when its replica
// identity is NOTHING: UPDATE and DELETE on it fail on a publisher, so a
// blue/green upgrade stops at it like at a table without a key.
func TestRun_ReplicaIdentityNothingWithPrimaryKey(t *testing.T) {
	d := newDB(t, "")
	requireBelow18(t, d)
	d.exec(t, "CREATE TABLE public.pknothing (id int PRIMARY KEY)",
		"ALTER TABLE public.pknothing REPLICA IDENTITY NOTHING",
		"CREATE TABLE public.pkdefault (id int PRIMARY KEY)",
		"CREATE TABLE public.uq (id int NOT NULL)",
		"CREATE UNIQUE INDEX uq_id ON public.uq (id)",
		"ALTER TABLE public.uq REPLICA IDENTITY USING INDEX uq_id")
	rep, err := Run(d.ctx, d.pool, Target{Major: 18, Method: BlueGreen}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	u1 := mustCheck(t, rep, "U1")
	if u1.Status != Block || u1.Total != 1 || len(u1.Rows) != 1 ||
		u1.Rows[0]["table"] != "public.pknothing" ||
		u1.Rows[0]["replica_identity"] != "nothing" {
		t.Fatalf("U1 = %+v, want a block on public.pknothing alone", u1)
	}
	if !strings.Contains(u1.Reason, "REPLICA IDENTITY NOTHING") {
		t.Fatalf("U1 reason = %q, want it to name REPLICA IDENTITY NOTHING", u1.Reason)
	}
}

// ftsFixture is the full text search indexes U5 must find on a non-libc
// database: GIN and GiST on a to_tsvector expression and a gin_trgm_ops
// index. A GIN index on a stored tsvector column and a btree on the
// expression are not listed: their keys are not re-derived by case
// folding of the new major.
func ftsFixture(t *testing.T, createOpts string) liveDB {
	d := newDB(t, createOpts)
	d.exec(t, "CREATE EXTENSION pg_trgm",
		"CREATE TABLE public.docs (id int PRIMARY KEY, body text, v tsvector)",
		"CREATE INDEX docs_fts_gin ON public.docs USING gin (to_tsvector('simple', body))",
		"CREATE INDEX docs_fts_gist ON public.docs USING gist "+
			"(to_tsvector('simple', body))",
		"CREATE INDEX docs_trgm_gist ON public.docs USING gist (body gist_trgm_ops)",
		"CREATE INDEX docs_v ON public.docs USING gin (v)",
		"CREATE INDEX docs_btree ON public.docs (to_tsvector('simple', body))")
	return d
}

func indexRows(c Check) []string {
	var out []string
	for _, r := range c.Rows {
		if r["kind"] == "index" {
			out = append(out, r["name"]+"/"+r["access_method"])
		}
	}
	sort.Strings(out)
	return out
}

func TestRun_FullTextSearchIndexesOnNonLibcDatabase(t *testing.T) {
	probe := newDB(t, "")
	requireBelow18(t, probe)
	if probe.major < 15 {
		// PG14 has no database-level locale provider: every database is libc,
		// which TestRun_FullTextSearchIndexesOnLibcDatabase covers.
		return
	}
	providers := []string{"LOCALE_PROVIDER icu ICU_LOCALE 'en-US'"}
	if probe.major >= 17 {
		providers = append(providers, "LOCALE_PROVIDER builtin BUILTIN_LOCALE 'C'")
	}
	want := "public.docs_fts_gin/gin,public.docs_fts_gist/gist,public.docs_trgm_gist/gist"
	for _, opts := range providers {
		d := ftsFixture(t, opts)
		rep, err := Run(d.ctx, d.pool, Target{Major: 18}, Options{})
		if err != nil {
			t.Fatalf("%s: Run: %v", opts, err)
		}
		u5 := mustCheck(t, rep, "U5")
		if got := strings.Join(indexRows(u5), ","); got != want || u5.Status != Warn {
			t.Fatalf("%s: U5 = %s with indexes %q, want warn listing %q", opts, u5.Status,
				got, want)
		}
		rep17, err := Run(d.ctx, d.pool, Target{Major: 17}, Options{})
		if probe.major < 17 {
			if err != nil {
				t.Fatalf("%s: Run to 17: %v", opts, err)
			}
			if got := indexRows(mustCheck(t, rep17, "U5")); len(got) != 0 {
				t.Fatalf("%s: target 17 listed %v, want the index arm skipped", opts, got)
			}
		}
	}
}

// On a libc database the case folding does not change, so the same
// indexes are not listed and the arm says why.
func TestRun_FullTextSearchIndexesOnLibcDatabase(t *testing.T) {
	d := ftsFixture(t, "")
	requireBelow18(t, d)
	rep, err := Run(d.ctx, d.pool, Target{Major: 18}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	u5 := mustCheck(t, rep, "U5")
	if got := indexRows(u5); len(got) != 0 ||
		!strings.Contains(strings.Join(u5.Skipped, ";"), "index: the database uses the libc") {
		t.Fatalf("U5 = %+v, want no index rows and the libc skip reason", u5)
	}
}
