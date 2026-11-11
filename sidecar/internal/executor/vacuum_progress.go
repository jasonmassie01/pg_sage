package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/catalogread"
)

// vacuumProgress is one pg_stat_progress_vacuum sample of a backend
// (vacuum-05). The dead-tuple columns are not read: PostgreSQL 17 renamed
// them. IndexesProcessed exists on PostgreSQL 17 and later only.
type vacuumProgress struct {
	Phase            string
	HeapBlksTotal    int64
	HeapBlksScanned  int64
	HeapBlksVacuumed int64
	IndexVacuumCount int64
	IndexesProcessed *int64
}

// same reports an unchanged progress tuple.
func (p vacuumProgress) same(o vacuumProgress) bool {
	return p.Phase == o.Phase && p.HeapBlksScanned == o.HeapBlksScanned &&
		p.HeapBlksVacuumed == o.HeapBlksVacuumed && p.IndexVacuumCount == o.IndexVacuumCount &&
		optionalEqual(p.IndexesProcessed, o.IndexesProcessed)
}

func optionalEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// heapPhase reports the phases with a heap block counter. Index phases are
// never judged stalled: before PostgreSQL 17 they have no counter, and one
// large index can take longer than the stall window on any version.
func (p vacuumProgress) heapPhase() bool {
	return p.Phase == "scanning heap" || p.Phase == "vacuuming heap"
}

func (p vacuumProgress) String() string {
	if p.Phase == "" {
		return "no progress row"
	}
	return fmt.Sprintf("phase %q, %d/%d heap blocks scanned, %d vacuumed, %d index passes",
		p.Phase, p.HeapBlksScanned, p.HeapBlksTotal, p.HeapBlksVacuumed, p.IndexVacuumCount)
}

// vacuumProgressSQL reads one backend's row. indexes_processed is read
// through to_jsonb, so the statement is the same on every version (NULL
// before PostgreSQL 17).
const vacuumProgressSQL = `/* pg_sage executor */ SELECT p.phase, p.heap_blks_total,
	p.heap_blks_scanned, p.heap_blks_vacuumed, p.index_vacuum_count,
	(to_jsonb(p) ->> 'indexes_processed')::bigint
	FROM pg_stat_progress_vacuum p WHERE p.pid = $1`

// vacuumProgressReader samples a VACUUM backend's progress; false when it
// has no progress row.
type vacuumProgressReader func(ctx context.Context, pid uint32) (vacuumProgress, bool, error)

// readVacuumProgress is the watchdog's bounded, read-only probe.
func (e *Executor) readVacuumProgress(
	ctx context.Context, pid uint32,
) (vacuumProgress, bool, error) {
	var p vacuumProgress
	var phase *string
	err := catalogread.New(e.pool, catalogread.Default()).QueryRow(ctx, vacuumProgressSQL,
		int32(pid)).Scan(&phase, &p.HeapBlksTotal, &p.HeapBlksScanned, &p.HeapBlksVacuumed,
		&p.IndexVacuumCount, &p.IndexesProcessed)
	if errors.Is(err, pgx.ErrNoRows) {
		return vacuumProgress{}, false, nil
	}
	if err != nil {
		return vacuumProgress{}, false, fmt.Errorf("read VACUUM progress of pid %d: %w", pid, err)
	}
	if phase != nil {
		p.Phase = *phase
	}
	return p, true, nil
}

// stallTracker reports a heap phase whose progress tuple has not changed
// for the stall window. A non-positive window never stalls.
type stallTracker struct {
	stall time.Duration
	last  vacuumProgress
	since time.Time
	seen  bool
}

func (s *stallTracker) observe(p vacuumProgress, now time.Time) bool {
	if !s.seen || !p.same(s.last) {
		s.last, s.since, s.seen = p, now, true
		return false
	}
	return s.stall > 0 && p.heapPhase() && now.Sub(s.since) >= s.stall
}

// reset forgets the last sample (the progress row was absent).
func (s *stallTracker) reset() { s.seen = false }
