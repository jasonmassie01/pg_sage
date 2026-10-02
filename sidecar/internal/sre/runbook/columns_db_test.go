package runbook

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The column vocabulary read from each probe's SQL is exactly what the
// probe returns on a real server, for every catalog probe (including any
// added later): a runbook can never name a column that does not exist, and
// is never refused one that does.
func TestColumnsOf_MatchesRealProbeColumns(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	var version int
	if err := pool.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	runner := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	for _, id := range probes.Catalog().IDs() {
		spec, _ := probes.Catalog().Spec(id)
		variant, ok := spec.VariantFor(version)
		if !ok {
			t.Errorf("probe %s has no variant for server %d", id, version)
			continue
		}
		res := runner.Run(ctx, id, argsFor(spec))
		if !res.Status.Usable() {
			t.Errorf("probe %s: %s (%s); cannot compare its columns", id, res.Status,
				res.Reason)
			continue
		}
		if got := ColumnsOf(variant.SQL); !reflect.DeepEqual(got, res.Columns) {
			t.Errorf("probe %s: columns read from SQL %v, server returned %v", id, got,
				res.Columns)
		}
	}
}

func argsFor(spec probes.Spec) probes.Args {
	if spec.Args == probes.ArgsBackend {
		return probes.Args{PID: 1, BackendStart: time.Now().UTC()}
	}
	return probes.Args{}
}
