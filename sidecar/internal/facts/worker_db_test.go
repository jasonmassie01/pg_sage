package facts

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeDetector struct {
	name      string
	proposals []Proposal
	err       error
}

func (d fakeDetector) Name() string { return d.name }
func (d fakeDetector) Detect(context.Context) ([]Proposal, error) {
	return d.proposals, d.err
}

func TestWorkerProposesNotifiesOnceAndKeepsGoing(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	var notified []int64
	chat := &recordingChatter{}
	now := time.Now()
	w := NewWorker(s, WorkerOptions{
		Detectors: []Detector{
			fakeDetector{name: "broken", err: errors.New("catalog read timed out")},
			fakeDetector{name: "fixtures", proposals: []Proposal{{Type: TypeTestFixture,
				Kind: KindSchema, Subject: "test_leak_*", Source: SourceDetector,
				Evidence: []Citation{citation("schemas:test_leak_*")}}}},
		},
		Model: NewModelProposer(chat),
		ModelEvidence: func(context.Context) ([]EvidenceItem, error) {
			return modelEvidence(), nil
		},
		ModelInterval: time.Hour,
		Notify:        func(_ context.Context, f Fact) { notified = append(notified, f.ID) },
		Now:           func() time.Time { return now },
	})
	first := w.RunOnce(ctx)
	if first.Proposed != 1 || first.Notified != 1 || len(first.Errors) != 1 ||
		chat.calls != 1 {
		t.Fatalf("first run %+v (model calls %d)", first, chat.calls)
	}
	second := w.RunOnce(ctx)
	if second.Proposed != 0 || second.Notified != 0 || chat.calls != 1 || len(notified) != 1 {
		t.Fatalf("second run %+v notified %v model calls %d", second, notified, chat.calls)
	}
	now = now.Add(2 * time.Hour)
	_ = w.RunOnce(ctx)
	if chat.calls != 2 {
		t.Fatalf("the model runs again after its interval: %d calls", chat.calls)
	}
	f, err := s.Get(ctx, notified[0])
	if err != nil || f.Status != StatusProposed || f.Proposals != 3 {
		t.Fatalf("fact after three runs: %+v %v", f, err)
	}
}

func TestWorkerImportsDeclaredContractsAndRecordsModelErrors(t *testing.T) {
	pool, ctx := livePool(t)
	execAll(t, ctx, pool, `INSERT INTO sage.slot_consumer_registry (slot_name, owner_tag)
		VALUES ('cdc_x', 'debezium')`)
	s := NewStore(pool)
	chat := &recordingChatter{err: errors.New("all retries failed: server error 429")}
	w := NewWorker(s, WorkerOptions{Model: NewModelProposer(chat),
		ModelEvidence: func(context.Context) ([]EvidenceItem, error) {
			return modelEvidence(), nil
		}})
	stats := w.RunOnce(ctx)
	if stats.Imported != 1 || len(stats.Errors) != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if again := w.RunOnce(ctx); again.Imported != 0 {
		t.Fatalf("imports once: %+v", again)
	}
}
