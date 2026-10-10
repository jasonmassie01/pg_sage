package agentposture

import (
	"strconv"
	"testing"
	"time"
)

// AP-10 on the test images (pgvector 0.8.2): HNSW and IVFFlat indexes are
// both below their fixes, so both arms are critical and name the indexes.
func TestAP10_PgvectorBelowFixWithIndexes(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE EXTENSION vector SCHEMA "+f.schema,
		"CREATE TABLE "+f.q("emb")+" (id int, v "+f.q("vector")+"(3))",
		"CREATE INDEX emb_hnsw ON "+f.q("emb")+" USING hnsw (v "+
			f.q("vector_l2_ops")+")",
		"CREATE INDEX emb_ivf ON "+f.q("emb")+" USING ivfflat (v "+
			f.q("vector_l2_ops")+") WITH (lists = 1)")
	var version string
	if err := f.pool.QueryRow(f.ctx, "SELECT extversion FROM pg_extension "+
		"WHERE extname = 'vector'").Scan(&version); err != nil {
		t.Fatalf("pgvector version: %v", err)
	}
	if version != "0.8.2" {
		t.Skipf("the fixture expects pgvector 0.8.2 (the CI image); found %s", version)
	}
	o := f.run("AP-10", f.env(nil))
	got := requireFinding(t, o, "vector/hnsw", Critical)
	requireContains(t, "AP-10 hnsw detail", got.Detail, "0.8.2", "0.8.4", f.q("emb_hnsw"))
	requireContains(t, "AP-10 hnsw fix", got.FixScript, "ALTER EXTENSION vector UPDATE")
	got = requireFinding(t, o, "vector/ivfflat", Critical)
	requireContains(t, "AP-10 ivfflat detail", got.Detail, "0.8.7", f.q("emb_ivf"))
}

// AP-10: pgvector without vector indexes is not reported.
func TestAP10_PgvectorWithoutIndexes(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE EXTENSION vector SCHEMA " + f.schema)
	o := f.run("AP-10", f.env(nil))
	requireNoFinding(t, o, "vector/hnsw")
	requireNoFinding(t, o, "vector/ivfflat")
}

// AP-10's end-of-life arm against the live server: the finding is there
// exactly when the server's major is within 90 days of (or past) its end
// of life today; on PG14 in October 2026 it is.
func TestAP10_ServerEndOfLifeOnLiveServer(t *testing.T) {
	f := newFixture(t)
	env := f.env(nil)
	o := f.run("AP-10", env)
	major := env.VersionNum / 10000
	object := "PostgreSQL " + strconv.Itoa(major)
	eol, known := postgresEOL[major]
	if !known {
		requireNoFinding(t, o, object)
		return
	}
	end, _ := time.Parse("2006-01-02", eol)
	if time.Until(end) <= 90*24*time.Hour {
		got := requireFinding(t, o, object, Warning)
		requireContains(t, "AP-10 EOL detail", got.Detail, eol)
		return
	}
	requireNoFinding(t, o, object)
}
