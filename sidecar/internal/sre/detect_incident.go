package sre

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
)

// Episode is one poll of an open reactive detector episode, handed to
// the episode sink so it becomes a sage.incidents row. IncidentID is the
// incident the episode was already recorded as (empty on its first
// poll, and until the sink records it): a sink refreshes that incident
// and never opens a second one for the same episode.
type Episode struct {
	Kind       TriggerKind
	Database   string
	Signal     string   // the detector's incident signal id
	Related    []string // RCA signals of the same family it may attach to
	Start      time.Time
	ObservedAt time.Time
	IncidentID string
	Severity   string
	Summary    string // the incident's root cause line
	Evidence   string // what was measured against which threshold
}

// EpisodeIncident is the incident an episode belongs to, as the Cases
// projection identifies it.
type EpisodeIncident struct {
	ID        string
	Database  string
	SignalIDs []string
	Source    string
}

// EpisodeSink records detector episodes as incidents: the database's RCA
// engine in production, which owns incident identity, persistence and
// notifications. It may return an incident with an error (the incident
// is tracked but its row is not written yet); an empty ID means nothing
// was recorded.
type EpisodeSink interface {
	RecordEpisode(ctx context.Context, ep Episode) (EpisodeIncident, error)
}

// detectorSeverity is an episode's incident severity. The detector's
// thresholds are conservative, so an episode is a warning; the RCA
// engine escalates an incident that keeps recurring.
const detectorSeverity = "warning"

// detectorSignals are the incident signal ids of detector episodes.
var detectorSignals = map[TriggerKind]string{TriggerCheckpoint: "sre_checkpoint_storm",
	TriggerTempFiles: "sre_temp_file_explosion", TriggerLWLock: "sre_lwlock_contention"}

var detectorSummaries = map[TriggerKind]string{
	TriggerCheckpoint: "Checkpoint storm: requested checkpoints outnumber timed ones",
	TriggerTempFiles:  "Temp-file explosion: temp-file writes passed the detector threshold",
	TriggerLWLock:     "LWLock contention: backends queue on one LWLock class",
}

// DetectorSignal is the incident signal id of a detector family's
// episodes; empty for families the detector does not watch.
func DetectorSignal(kind TriggerKind) string { return detectorSignals[kind] }

// relatedSignals are the RCA signals that start the same family.
func relatedSignals(kind TriggerKind) []string {
	var out []string
	for sig, k := range signalKinds {
		if k == kind && sig != detectorSignals[kind] {
			out = append(out, sig)
		}
	}
	sort.Strings(out)
	return out
}

// WithIncidents records every episode through sink and links its
// triggers to the incident. Call it before the first poll.
func (d *ReactiveDetector) WithIncidents(sink EpisodeSink) *ReactiveDetector {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sink = sink
	return d
}

// trigger records the poll of an open episode and returns its trigger:
// linked to the episode's incident once one is recorded, the detector's
// own trigger before that (the investigation never waits for the
// incident store).
func (d *ReactiveDetector) trigger(ctx context.Context, kind TriggerKind, e *episode,
	rd reading) Trigger {
	if d.sink != nil {
		d.record(ctx, kind, e, rd)
	}
	if e.incident.ID == "" {
		return d.legacyTrigger(kind, e)
	}
	return incidentTrigger(cases.SourceIncident{ID: e.incident.ID,
		DatabaseName: e.incident.Database, SignalIDs: e.incident.SignalIDs,
		Source: e.incident.Source}, kind)
}

func (d *ReactiveDetector) record(ctx context.Context, kind TriggerKind, e *episode,
	rd reading) {
	inc, err := d.sink.RecordEpisode(ctx, Episode{Kind: kind, Database: d.database,
		Signal: detectorSignals[kind], Related: relatedSignals(kind), Start: e.start,
		ObservedAt: rd.at, IncidentID: e.incident.ID, Severity: detectorSeverity,
		Summary: detectorSummaries[kind], Evidence: rd.evidence})
	if err != nil {
		d.notices.Log(d.logFn, fmt.Sprintf("episode/%s/%v", kind, err), "WARN",
			"sre: db %q: recording the %s episode as an incident: %v; the investigation "+
				"still starts", d.database, kind, err)
	}
	if e.incident.ID == "" && inc.ID != "" {
		e.incident = inc
	}
}

// incidentTrigger is the trigger of an open incident: its Cases identity,
// one investigation per incident (CHECK-13).
func incidentTrigger(inc cases.SourceIncident, kind TriggerKind) Trigger {
	return Trigger{CaseID: clip(cases.IncidentIdentityKey(inc)), IncidentID: inc.ID,
		Kind: kind, Subject: "incident " + inc.ID, IdempotencyKey: "incident:" + inc.ID}
}
