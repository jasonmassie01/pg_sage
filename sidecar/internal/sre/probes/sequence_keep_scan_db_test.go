package probes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// forecast-10: a kept sequence is read first under the scan cap, so a
// sampled sequence keeps its series even when the catalog holds more
// sequences with less capacity than one statement may read.

// No concurrent access tests: one statement reads one catalog snapshot.

func TestSequenceRunway_KeptSequenceIsReadPastTheScanCap(t *testing.T) {
	pool, ctx := livePool(t)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	sch := sliceFixture(t, ctx, pool, SequenceScanCap+600)
	// Created last as a default bigint, so it has the most capacity and the
	// highest OID: the capacity order reads it after SequenceScanCap others.
	q := pgx.Identifier{sch}.Sanitize()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s.zz_kept AS bigint;
		SELECT nextval('%[1]s.zz_kept')`, q)); err != nil {
		t.Fatalf("kept sequence: %v", err)
	}
	plain := catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{})
	if _, ok := sequencesIn(t, plain, sch)["zz_kept"]; ok {
		t.Fatal("zz_kept read without a keep-list; the fixture does not pass the scan cap")
	}
	kept := catalogRun(t, ctx, pool, SequenceRunwayProbe,
		Args{Keep: []string{sch + ".zz_kept"}})
	if s, ok := sequencesIn(t, kept, sch)["zz_kept"]; !ok || s.LastValue != 1 {
		t.Fatalf("kept sequence = %+v (%v), want it read first and listed", s, ok)
	}
	c, err := SequenceCoverageOf(kept)
	if err != nil || !c.ScanCapped() || c.Scanned > SequenceScanCap {
		t.Fatalf("coverage = %+v (%v), want the scan still capped", c, err)
	}
}
