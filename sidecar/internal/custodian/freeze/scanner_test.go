package freeze

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScannerReadsEveryEligibleDatabaseBeforePublishingProposals(t *testing.T) {
	catalog := &fakeCatalog{databases: []DatabaseRef{
		{Name: "orders", AllowConnections: true},
		{Name: "analytics", AllowConnections: true},
		{Name: "template0", AllowConnections: false, IsTemplate: true},
		{Name: "retired", AllowConnections: false},
	}}
	reader := &fakeHorizonReader{samples: map[string][]HorizonSample{
		"orders":    {redXIDSample("orders", "public", "events")},
		"analytics": {amberMultiXactSample("analytics", "public", "facts")},
	}}
	sink := &recordingProposalSink{}
	scanner := NewScanner(catalog, reader, sink, ScannerOptions{
		Thresholds: Thresholds{RedBufferPct: 25, AmberBufferPct: 50},
		Now: func() time.Time {
			return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
		},
	})

	result, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !reflect.DeepEqual(reader.calls, []string{"orders", "analytics"}) {
		t.Fatalf("reader calls = %#v, want every eligible database", reader.calls)
	}
	if result.DatabasesDiscovered != 4 || result.DatabasesScanned != 2 ||
		result.DatabasesSkipped != 2 || result.ProposalsPublished != 2 {
		t.Fatalf("ScanResult = %#v", result)
	}
	if result.ExecutedActions != 0 {
		t.Fatalf("ExecutedActions = %d, want 0", result.ExecutedActions)
	}
	if len(sink.proposals) != 2 {
		t.Fatalf("proposals = %#v", sink.proposals)
	}
	assertSafeProposal(t, sink.proposals[0], "orders", ThreatXID, UrgencyRed)
	assertSafeProposal(t, sink.proposals[1],
		"analytics", ThreatMultiXact, UrgencyAmber)
}

func TestScannerFailsClosedWithoutPartialProposalsOnDatabaseError(t *testing.T) {
	probeErr := errors.New("permission denied reading pg_class")
	catalog := &fakeCatalog{databases: []DatabaseRef{
		{Name: "orders", AllowConnections: true},
		{Name: "broken", AllowConnections: true},
		{Name: "analytics", AllowConnections: true},
	}}
	reader := &fakeHorizonReader{
		samples: map[string][]HorizonSample{
			"orders": {redXIDSample("orders", "public", "events")},
		},
		errors: map[string]error{"broken": probeErr},
	}
	sink := &recordingProposalSink{}
	scanner := NewScanner(catalog, reader, sink, defaultScannerOptions())

	result, err := scanner.Scan(context.Background())

	if !errors.Is(err, probeErr) || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("Scan error = %v", err)
	}
	if len(sink.proposals) != 0 || result.ProposalsPublished != 0 ||
		result.ExecutedActions != 0 {
		t.Fatalf("failed scan leaked work: result=%#v proposals=%#v",
			result, sink.proposals)
	}
}

func TestScannerFailsClosedWhenCatalogOrProposalSinkFails(t *testing.T) {
	catalogErr := errors.New("pg_database unavailable")
	scanner := NewScanner(
		&fakeCatalog{err: catalogErr}, &fakeHorizonReader{},
		&recordingProposalSink{}, defaultScannerOptions(),
	)
	result, err := scanner.Scan(context.Background())
	if !errors.Is(err, catalogErr) || result.ProposalsPublished != 0 {
		t.Fatalf("catalog failure = result %#v error %v", result, err)
	}

	sinkErr := errors.New("ledger unavailable")
	sink := &recordingProposalSink{err: sinkErr}
	scanner = NewScanner(
		&fakeCatalog{databases: []DatabaseRef{{Name: "orders", AllowConnections: true}}},
		&fakeHorizonReader{samples: map[string][]HorizonSample{
			"orders": {redXIDSample("orders", "public", "events")},
		}}, sink, defaultScannerOptions(),
	)
	result, err = scanner.Scan(context.Background())
	if !errors.Is(err, sinkErr) || result.ProposalsPublished != 0 ||
		result.ExecutedActions != 0 {
		t.Fatalf("sink failure = result %#v error %v", result, err)
	}
}

func TestScannerPropagatesCancellationWithoutMutation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := &recordingProposalSink{}
	scanner := NewScanner(
		&fakeCatalog{databases: []DatabaseRef{{Name: "orders", AllowConnections: true}}},
		&fakeHorizonReader{}, sink, defaultScannerOptions(),
	)

	result, err := scanner.Scan(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan error = %v, want context.Canceled", err)
	}
	if len(sink.proposals) != 0 || result.ExecutedActions != 0 {
		t.Fatalf("cancelled scan leaked work: %#v %#v", result, sink.proposals)
	}
}

func assertSafeProposal(
	t *testing.T, proposal Proposal,
	database string, threat ThreatKind, urgency Urgency,
) {
	t.Helper()
	if proposal.Database != database || proposal.Threat != threat ||
		proposal.Urgency != urgency || proposal.Intent != IntentVacuumFreeze {
		t.Fatalf("Proposal = %#v", proposal)
	}
	if !proposal.RequiresAuthorization || proposal.ExecuteDirectly {
		t.Fatalf("unsafe proposal authorization flags = %#v", proposal)
	}
	if !strings.HasPrefix(proposal.SQL, "VACUUM (FREEZE) ") ||
		strings.Contains(strings.ToUpper(proposal.SQL), "VACUUM FULL") {
		t.Fatalf("unsafe freeze SQL = %q", proposal.SQL)
	}
	if proposal.Deadline.Kind != DeadlineXID || proposal.Deadline.HardAt.IsZero() {
		t.Fatalf("proposal deadline = %#v", proposal.Deadline)
	}
}

type fakeCatalog struct {
	databases []DatabaseRef
	err       error
}

func (catalog *fakeCatalog) ListDatabases(
	ctx context.Context,
) ([]DatabaseRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]DatabaseRef(nil), catalog.databases...), catalog.err
}

type fakeHorizonReader struct {
	samples map[string][]HorizonSample
	errors  map[string]error
	calls   []string
}

func (reader *fakeHorizonReader) ReadHorizons(
	ctx context.Context, database DatabaseRef,
) ([]HorizonSample, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader.calls = append(reader.calls, database.Name)
	if err := reader.errors[database.Name]; err != nil {
		return nil, err
	}
	return append([]HorizonSample(nil), reader.samples[database.Name]...), nil
}

type recordingProposalSink struct {
	proposals []Proposal
	err       error
}

func (sink *recordingProposalSink) Publish(
	ctx context.Context, proposals []Proposal,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sink.err != nil {
		return sink.err
	}
	sink.proposals = append(sink.proposals, proposals...)
	return nil
}

func defaultScannerOptions() ScannerOptions {
	return ScannerOptions{
		Thresholds: Thresholds{RedBufferPct: 25, AmberBufferPct: 50},
		Now:        time.Now,
	}
}

func redXIDSample(database, schema, table string) HorizonSample {
	return HorizonSample{
		Database: database, Schema: schema, Table: table,
		XIDAge: 180, XIDMaxAge: 200, XIDsPerSecond: 10,
		MultiXactAge: 10, MultiXactMaxAge: 1000,
		MultiXactsPerSecond: 1,
	}
}

func amberMultiXactSample(database, schema, table string) HorizonSample {
	return HorizonSample{
		Database: database, Schema: schema, Table: table,
		XIDAge: 10, XIDMaxAge: 1000, XIDsPerSecond: 1,
		MultiXactAge: 120, MultiXactMaxAge: 200,
		MultiXactsPerSecond: 10,
	}
}

var _ DatabaseCatalog = (*fakeCatalog)(nil)
var _ HorizonReader = (*fakeHorizonReader)(nil)
var _ ProposalSink = (*recordingProposalSink)(nil)
