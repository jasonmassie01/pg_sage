package runway

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Fleet WAL-runway dedupe (Sage SRE follow-ups B). The databases' total
// size is a cluster-level number, but every fleet runtime on a cluster
// used to sum every database's size on every pass: N runtimes x N
// databases. A SizeShare, one per process, lets one runtime measure each
// cluster once per pass and the others reuse that reading; runtimes that
// tick together wait for the one measurement in flight.

// ClusterSize is one measurement of the databases' total size.
type ClusterSize struct {
	DatabaseBytes       float64
	UnreadableDatabases float64
	MeasuredAt          time.Time
}

// SizeShare shares cluster size measurements between runtimes.
type SizeShare struct {
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]*sizeEntry
}

// sizeEntry is a cluster's last good reading and its flight in progress.
type sizeEntry struct {
	last   ClusterSize
	has    bool
	flight *sizeFlight
}

type sizeFlight struct {
	done chan struct{}
	size ClusterSize
	err  error
}

// NewSizeShare returns an empty share.
func NewSizeShare() *SizeShare {
	return &SizeShare{now: time.Now, entries: map[string]*sizeEntry{}}
}

// ClusterKey names the cluster a wal_runway reading came from: its system
// identifier, plus the postmaster start (a standby or a restored clone
// shares the identifier, never the start) and the role (the readable
// databases depend on it). Without a system identifier there is no key.
func ClusterKey(w probes.WALRunway) string {
	if w.SystemID == "" {
		return ""
	}
	return strings.Join([]string{w.SystemID, w.StartedAt.UTC().Format(time.RFC3339Nano),
		w.RoleName}, "/")
}

// Measure returns key's reading when it is younger than pass, or
// measures it (once, however many runtimes ask at the same time). A
// failed measurement is returned to everyone waiting on it and never
// reused. An empty key, a pass of zero or a nil share always measure.
// shared reports a reading another call measured.
func (s *SizeShare) Measure(ctx context.Context, key string, pass time.Duration,
	measure func(context.Context) (ClusterSize, error)) (ClusterSize, bool, error) {
	if s == nil || key == "" || pass <= 0 {
		size, err := s.measureNow(ctx, measure)
		return size, false, err
	}
	s.mu.Lock()
	e := s.entries[key]
	if e == nil {
		s.pruneLocked(pass)
		e = &sizeEntry{}
		s.entries[key] = e
	}
	if e.has && s.now().Sub(e.last.MeasuredAt) < pass {
		size := e.last
		s.mu.Unlock()
		return size, true, nil
	}
	if f := e.flight; f != nil {
		s.mu.Unlock()
		return waitFlight(ctx, f)
	}
	f := &sizeFlight{done: make(chan struct{})}
	e.flight = f
	s.mu.Unlock()
	f.size, f.err = s.measureNow(ctx, measure)
	s.mu.Lock()
	e.flight = nil
	if f.err == nil {
		e.last, e.has = f.size, true
	}
	s.mu.Unlock()
	close(f.done)
	return f.size, false, f.err
}

// maxSizeEntries bounds the clusters a share remembers; a restart or a
// failover gives a cluster a new key, so old keys are dropped once stale.
const maxSizeEntries = 256

// pruneLocked drops readings older than a pass that have no flight in
// progress, once the share holds maxSizeEntries clusters.
func (s *SizeShare) pruneLocked(pass time.Duration) {
	if len(s.entries) < maxSizeEntries {
		return
	}
	for k, e := range s.entries {
		if e.flight == nil && (!e.has || s.now().Sub(e.last.MeasuredAt) >= pass) {
			delete(s.entries, k)
		}
	}
}

func waitFlight(ctx context.Context, f *sizeFlight) (ClusterSize, bool, error) {
	select {
	case <-f.done:
		return f.size, f.err == nil, f.err
	case <-ctx.Done():
		return ClusterSize{}, false, ctx.Err()
	}
}

// measureNow measures and stamps the reading with the share's clock.
func (s *SizeShare) measureNow(ctx context.Context,
	measure func(context.Context) (ClusterSize, error)) (ClusterSize, error) {
	size, err := measure(ctx)
	if err != nil {
		return ClusterSize{}, err
	}
	if s != nil {
		size.MeasuredAt = s.now()
	} else {
		size.MeasuredAt = time.Now()
	}
	return size, nil
}

// measureSize runs cluster_database_size.
func measureSize(ctx context.Context, r ProbeRunner) (ClusterSize, error) {
	bytes, unreadable, err := probes.ClusterSizeOf(r.Run(ctx, probes.ClusterDatabaseSizeProbe,
		probes.Args{}))
	return ClusterSize{DatabaseBytes: bytes, UnreadableDatabases: unreadable}, err
}
