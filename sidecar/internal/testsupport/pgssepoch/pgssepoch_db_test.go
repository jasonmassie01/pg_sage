package pgssepoch

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-n: under the pinned path (pg_catalog, pg_temp) the epoch is
// read through the extension's own schema, so a pinned test is measured,
// not skipped as "unavailable".
func TestEpoch_ReadsThroughTheExtensionSchemaUnderThePin(t *testing.T) {
	_, pinned := testdb.ExtSchemaDatabase(t, "pgssepoch_pin")
	epoch, err := Epoch(context.Background(), pinned)
	if err != nil || !strings.Contains(epoch, "/") {
		t.Fatalf("epoch = %q err = %v, want stats_reset/dealloc", epoch, err)
	}
}

func TestEpoch_AbsentExtensionIsAnError(t *testing.T) {
	pinned := testdb.PinnedPool(t, testdb.CreateDatabase(t, "pgssepoch_absent"))
	if _, err := pinned.Exec(context.Background(),
		"DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatal(err)
	}
	epoch, err := Epoch(context.Background(), pinned)
	if err == nil || epoch != "" || !strings.Contains(err.Error(), "pg_stat_statements_info") {
		t.Fatalf("epoch = %q err = %v, want an error naming the view", epoch, err)
	}
}
