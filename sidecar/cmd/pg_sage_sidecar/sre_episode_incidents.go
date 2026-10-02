package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/sre"
)

// episodePersistTimeout bounds one persistence pass of detector
// incidents (insert, LLM narration of the notification, dispatch).
const episodePersistTimeout = 60 * time.Second

// episodePersistRetry is the wait before a failed pass is retried.
const episodePersistRetry = 5 * time.Second

// episodeIncidents records reactive detector episodes as incidents of the
// database's RCA engine, the single owner of incident state. The engine
// tracks the incident at once, so the investigation links to it on the
// same trigger poll; persistence and the incident notification (which may
// wait for narration) run on the incident worker, off the poll.
type episodeIncidents struct {
	eng   *rca.Engine
	pool  *pgxpool.Pool
	logFn func(string, string, ...any)
	wake  chan struct{}
	retry time.Duration
}

var _ sre.EpisodeSink = (*episodeIncidents)(nil)

// newEpisodeIncidents binds the engine to the database's incident store;
// Run persists what RecordEpisode tracks.
func newEpisodeIncidents(eng *rca.Engine, pool *pgxpool.Pool,
	logFn func(string, string, ...any)) *episodeIncidents {
	return &episodeIncidents{eng: eng, pool: pool, logFn: logFn, wake: make(chan struct{}, 1),
		retry: episodePersistRetry}
}

// RecordEpisode implements sre.EpisodeSink.
func (s *episodeIncidents) RecordEpisode(ctx context.Context, ep sre.Episode) (
	sre.EpisodeIncident, error) {
	if err := s.eng.Hydrate(ctx, s.pool); err != nil {
		return sre.EpisodeIncident{}, fmt.Errorf("load open incidents: %w", err)
	}
	inc, ok := s.eng.ObserveEpisode(ctx, rca.Episode{Signal: ep.Signal,
		Related: ep.Related, Severity: ep.Severity, RootCause: ep.Summary,
		Evidence: ep.Evidence, ObservedAt: ep.ObservedAt, IncidentID: ep.IncidentID})
	if !ok {
		return sre.EpisodeIncident{}, nil
	}
	select {
	case s.wake <- struct{}{}:
	default: // a persistence pass is already pending
	}
	return sre.EpisodeIncident{ID: inc.ID, Database: inc.DatabaseName,
		SignalIDs: append([]string(nil), inc.SignalIDs...), Source: inc.Source}, nil
}

// Run persists tracked incidents (and sends their notifications) after
// each recorded episode until ctx ends; a failed pass is retried.
func (s *episodeIncidents) Run(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-retry:
		}
		retry = nil
		if !s.persist(ctx) && ctx.Err() == nil {
			retry = time.After(s.retry)
		}
	}
}

func (s *episodeIncidents) persist(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, episodePersistTimeout)
	defer cancel()
	if err := s.eng.PersistIncidents(ctx, s.pool); err != nil {
		s.logFn("WARN", "sre: persisting detector incidents failed (retried in %s): %v",
			s.retry, err)
		return false
	}
	return true
}

// detectorIncidents is the database's episode sink: its RCA engine and
// incident store, persisted on the instance worker group. Nil without
// RCA (rca.enabled: false), so detector episodes keep their own case.
func (rt *databaseRuntime) detectorIncidents() sre.EpisodeSink {
	if rt.rca == nil {
		return nil
	}
	sink := newEpisodeIncidents(rt.rca, rt.spec.Pool, logStructuredWrapper)
	rt.start(func() { sink.Run(rt.ctx) })
	return sink
}
