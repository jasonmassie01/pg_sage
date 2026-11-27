package earned

import (
	"context"
	"time"
)

// Perf (earned-08, 2026-11-05): the Autonomy view and Evaluate read each
// pair's level and evidence one after another (150+ round trips per view,
// polled every 30 s). They now queue the same reads as the Trust view,
// every one set-based, and send them in one pipelined round trip.

// ledgerReads are the reads of a whole-grid view or evaluation: the
// pending proposals, every stored level and the pairs' evidence.
type ledgerReads struct {
	pending []Proposal
	levels  []State
	e       *evidenceSet
	byPair  map[pairKey]State
}

// queueLedger queues on b the ledger reads of pairs; they are complete
// once b has run.
func (s *Service) queueLedger(ctx context.Context, b *readBatch, pairs []pairKey,
	need evidenceNeeds, now time.Time) (*ledgerReads, error) {
	in := &ledgerReads{e: newEvidenceSet(now)}
	for _, queue := range []func() error{
		func() error { return s.store.readProposals(ctx, b, StatusPending, 200, &in.pending) },
		func() error { return s.store.readLevels(ctx, b, &in.levels) },
		func() error { return s.queueEvidence(ctx, b, pairs, need, in.e) },
	} {
		if err := queue(); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// granted is a pair's level as Granted reads it, from the read levels:
// its stored row or its default, a composite bounded by its components.
func (in *ledgerReads) granted(f Family, c ActionClass) State {
	if in.byPair == nil {
		in.byPair = levelMap(in.levels)
	}
	st, found := in.byPair[pairKey{f, c}]
	if !found {
		st = defaultState(f, c)
	}
	return composedLevel(st, found, func(p Pair) Level {
		return in.granted(p.Family, p.Class).Level
	})
}

// pendingByPair indexes the pending proposals by pair.
func (in *ledgerReads) pendingByPair() map[pairKey]*Proposal {
	out := map[pairKey]*Proposal{}
	for i := range in.pending {
		out[pairKey{in.pending[i].Family, in.pending[i].Class}] = &in.pending[i]
	}
	return out
}

// viewPairs are the pairs the Autonomy view shows: every incident family's
// applicable classes, in order.
func viewPairs() []pairKey {
	var out []pairKey
	for _, f := range Families() {
		for _, c := range ApplicableClasses(f) {
			out = append(out, pairKey{f, c})
		}
	}
	return out
}
