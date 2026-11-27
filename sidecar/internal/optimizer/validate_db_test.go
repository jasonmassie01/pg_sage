package optimizer

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// indexes-01: checkDuplicate compares the model's raw DDL with
// pg_get_indexdef, which omits a default opclass and a collation equal to
// the column's. With the catalog's defaults in the table context an
// explicit default still reads as the existing index, while an index that
// serves other operators is admitted.
func TestCheckDuplicate_CatalogDefaultsOnRealPostgres(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	snap := opclassFixture(t, pool)
	o := New(pool, &config.OptimizerConfig{}, noopLog2)
	tc := opclassDBContext(t, o, snap)
	for _, c := range []struct {
		keys string
		dup  bool
	}{
		{"(email text_ops)", true},
		{`(email COLLATE "default")`, true},
		{`(email COLLATE pg_catalog."default" text_ops)`, true},
		{"(email DESC)", true},
		{"(email text_pattern_ops)", false},
		{`(email COLLATE "C")`, false},
		{"(email, id DESC)", false},
	} {
		ok, reason := o.validator.checkDuplicate(Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY c ON opclass_probe.people " + c.keys}, tc)
		if ok == c.dup {
			t.Errorf("%s: ok=%v (%s), want duplicate=%v", c.keys, ok, reason, c.dup)
		}
		if c.dup && !strings.Contains(reason, "people_email") {
			t.Errorf("%s: reason %q does not name people_email", c.keys, reason)
		}
	}
}

// pgvector: HNSW has no default opclass for vector, so pg_get_indexdef
// spells vector_l2_ops out, and a cosine index beside it is not a
// duplicate. A varchar column's default (text_ops, binary compatible) is
// resolved the way PostgreSQL resolves it.
func TestCheckDuplicate_VectorAndVarcharOnRealPostgres(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	var available bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS
		(SELECT 1 FROM pg_available_extensions WHERE name = 'vector')`).
		Scan(&available); err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Skip("pgvector server extension unavailable; HNSW opclass pair not verified")
	}
	snap := opclassFixture(t, pool,
		"CREATE EXTENSION IF NOT EXISTS vector",
		"ALTER TABLE opclass_probe.people ADD COLUMN embedding vector(3)",
		"CREATE INDEX people_emb_l2 ON opclass_probe.people "+
			"USING hnsw (embedding vector_l2_ops)",
		"CREATE INDEX people_vc ON opclass_probe.people (vc)")
	o := New(pool, &config.OptimizerConfig{}, noopLog2)
	tc := opclassDBContext(t, o, snap)
	for _, c := range []struct {
		keys string
		dup  bool
	}{
		{"USING hnsw (embedding vector_l2_ops)", true},
		{"USING hnsw (embedding vector_cosine_ops)", false},
		{"(vc text_ops)", true},
		{"(vc varchar_pattern_ops)", false},
	} {
		ok, reason := o.validator.checkDuplicate(Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY c ON opclass_probe.people " + c.keys}, tc)
		if ok == c.dup {
			t.Errorf("%s: ok=%v (%s), want duplicate=%v", c.keys, ok, reason, c.dup)
		}
	}
}

// pg_get_indexdef quotes an operator class parameter (siglen='32'); the
// model writes siglen=32. Both name the same index, and siglen=64 does not.
func TestCheckDuplicate_OpclassParametersOnRealPostgres(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	snap := opclassFixture(t, pool,
		"ALTER TABLE opclass_probe.people ADD COLUMN t tsvector",
		"CREATE INDEX people_t_sig ON opclass_probe.people "+
			"USING gist (t tsvector_ops (siglen = 32))")
	o := New(pool, &config.OptimizerConfig{}, noopLog2)
	tc := opclassDBContext(t, o, snap)
	for _, c := range []struct {
		keys string
		dup  bool
	}{
		{"USING gist (t tsvector_ops(siglen=32))", true},
		{"USING gist (t tsvector_ops(siglen='32'))", true},
		{"USING gist (t tsvector_ops(siglen=64))", false},
		{"USING gist (t)", false},
	} {
		ok, reason := o.validator.checkDuplicate(Recommendation{
			DDL: "CREATE INDEX CONCURRENTLY c ON opclass_probe.people " + c.keys}, tc)
		if ok == c.dup {
			t.Errorf("%s: ok=%v (%s), want duplicate=%v", c.keys, ok, reason, c.dup)
		}
		if c.dup && !strings.Contains(reason, "people_t_sig") {
			t.Errorf("%s: reason %q does not name people_t_sig", c.keys, reason)
		}
	}
}
