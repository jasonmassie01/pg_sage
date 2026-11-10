package siem

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// UndeliverableLabel marks the stub sent in place of an event the
// receiver refuses (metadata.labels).
const UndeliverableLabel = "undeliverable"

// errPoisoned is a lane whose receiver refuses an event and its stub: the
// cursor holds before it until the receiver takes one of them.
var errPoisoned = errors.New("siem: lane poisoned")

// Undeliverable is an event a sink refused, replaced by its stub.
type Undeliverable struct {
	Sink   string
	Source string
	Chain  string
	Seq    int64
	Hash   string
	Reason string // the receiver's refusal
}

// laneRun is one drain of one chain of one source to one sink.
type laneRun struct {
	s     *sinkState
	src   Source
	chain string
	fence Fence
	seq   int64 // the cursor
}

// deliver sends recs' events in order and moves the cursor past each part
// the receiver takes. A permanent refusal is narrowed by halving the batch
// down to the single event refused, which is replaced by its stub; a
// transient failure stops with the cursor after the last part taken.
func (e *Exporter) deliver(ctx context.Context, r *laneRun, recs []Record,
	events []Event) error {
	err := r.s.cfg.Sink.Send(ctx, events)
	if err == nil {
		return e.commit(ctx, r, recs, false)
	}
	if !errors.Is(err, ErrPermanent) || ctx.Err() != nil {
		return err
	}
	if len(recs) == 1 {
		return e.undeliverable(ctx, r, recs[0], err)
	}
	mid := len(recs) / 2
	if err := e.deliver(ctx, r, recs[:mid], events[:mid]); err != nil {
		return err
	}
	return e.deliver(ctx, r, recs[mid:], events[mid:])
}

// undeliverable sends rec's stub, reports it (OnUndeliverable), then moves
// the cursor past it. A failed report holds the cursor, so the stub is
// sent and reported again.
func (e *Exporter) undeliverable(ctx context.Context, r *laneRun, rec Record,
	refusal error) error {
	if err := r.s.cfg.Sink.Send(ctx, []Event{stubEvent(rec)}); err != nil {
		if errors.Is(err, ErrPermanent) {
			return poisonedError(rec.Link.Seq, refusal, err)
		}
		return err
	}
	u := Undeliverable{Sink: r.s.cfg.Sink.Name(), Source: r.src.Name, Chain: r.chain,
		Seq: rec.Link.Seq, Hash: rec.Link.Hash, Reason: refusal.Error()}
	if e.opts.OnUndeliverable != nil {
		if err := e.opts.OnUndeliverable(ctx, u); err != nil {
			return fmt.Errorf("link %d was sent as a stub, but recording it failed, so "+
				"it is sent again: %w", u.Seq, err)
		}
	}
	e.logf("siem sink %s: %s: the receiver refuses link %d (%v); a stub labelled %q "+
		"with its position and hashes was sent instead, and the event stays in the "+
		"database", u.Sink, laneName(u.Source, u.Chain), u.Seq, refusal, UndeliverableLabel)
	return e.commit(ctx, r, []Record{rec}, true)
}

func poisonedError(seq int64, refusal, stubRefusal error) error {
	return fmt.Errorf("%w: link %d: the receiver refuses the event (%v) and its stub "+
		"(%v); export of this chain holds there until it takes one", errPoisoned, seq,
		refusal, stubRefusal)
}

// commit moves the cursor past recs, delivered (or stubbed).
func (e *Exporter) commit(ctx context.Context, r *laneRun, recs []Record,
	stubbed bool) error {
	last := recs[len(recs)-1].Link
	if err := e.cursors.Save(ctx, r.fence, r.s.cfg.Sink.Name(), r.src.Name, r.chain,
		last.Seq, last.Hash); err != nil {
		return err
	}
	r.seq = last.Seq
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.status.Delivered += int64(len(recs))
	if stubbed {
		r.s.status.Undeliverable++
	}
	r.s.status.LastSuccess = time.Now().UTC()
	l := r.s.lane(r.src.Name, r.chain)
	l.track(last.Seq, l.head)
	return nil
}

// stubEvent stands in for an event the receiver refuses: its class and
// type, status Failure, at least Medium severity, its link's position, op
// and hashes, and the "undeliverable" label. The row, the query and every
// other content field are left out; they stay in the database, where the
// link hash proves them.
func stubEvent(r Record) Event {
	m := classify(r)
	uid := r.Source + "/" + r.Chain + "/" + strconv.FormatInt(r.Link.Seq, 10)
	labels := append(append([]string{}, r.Labels...), UndeliverableLabel)
	sev := max(m.severity, SeverityMedium)
	return Event{
		"class_uid": m.class, "class_name": classNames[m.class],
		"category_uid": m.class / 1000, "category_name": categoryNames[m.class/1000],
		"activity_id": m.activity, "activity_name": m.activityName,
		"type_uid":    m.class*100 + m.activity,
		"type_name":   classNames[m.class] + ": " + m.activityName,
		"severity_id": sev, "severity": severityNames[sev],
		"status_id": statusFailure, "status": statusNames[statusFailure],
		"time": r.Link.At.UnixMilli(),
		"message": "pg_sage audit event " + uid + " was refused by this receiver; its " +
			"content stays in the database and its chain link is below",
		"metadata": map[string]any{"version": OCSFVersion, "log_name": r.Chain, "uid": uid,
			"logged_time": r.Link.At.UnixMilli(), "labels": labels,
			"product": map[string]any{"name": "pg_sage", "vendor_name": "pg_sage"}},
		"unmapped": map[string]any{"pg_sage": map[string]any{"source": r.Source,
			"chain": r.Chain, "seq": r.Link.Seq, "op": r.Link.Op, "row_id": r.Link.RowID,
			"v": r.Link.V, "hash": r.Link.Hash, "prev_hash": r.Link.PrevHash,
			"sealed_hash": r.Link.SealedHash, "state": r.Link.State}},
	}
}
