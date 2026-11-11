package auditchain

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// newestPruneSQL reads a chain's newest prune link (the marker index).
const newestPruneSQL = `/* pg_sage audit_chain v1 */ SELECT seq, state
	FROM sage.audit_chain_link WHERE chain = $1 AND op = 'P' ORDER BY seq DESC LIMIT 1`

// pruned is a chain's newest prune link; floorStart says the verification
// starts at its floor and so rests on its checkpoint.
type pruned struct {
	seq        int64
	mark       PruneMark
	floorStart bool
	anchorOK   bool   // the anchoring checkpoint link verified
	anchorWhy  string // why it did not, when known
}

// readPrune reads the newest prune link. An unreadable one is a problem,
// and the verification then starts at 1 (finding the pruned links gone).
func (v *verifier) readPrune(ctx context.Context) error {
	var seq int64
	var state string
	err := v.q.QueryRow(ctx, newestPruneSQL, v.s.Chain).Scan(&seq, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read prune link: %w", err)
	}
	mark, err := ParsePruneMark(state)
	if err == nil && mark.CheckpointLink >= seq {
		err = fmt.Errorf("its checkpoint link %d does not precede it", mark.CheckpointLink)
	}
	if err != nil {
		v.rep.add(Problem{Kind: KindSignedCheckpoint, Seq: seq,
			Detail: fmt.Sprintf("the prune link is unreadable (%v)", err)})
		return nil
	}
	v.prune = &pruned{seq: seq, mark: mark}
	return nil
}

// startsAtFloor clamps a verification that would start below a prune's
// floor to the floor itself, when pruning is enabled. Otherwise the walk
// starts where it was asked to and reports the pruned links missing, and
// the prune link is a problem: with the switch off nothing pg_sage did
// wrote it, and honouring it would let anyone able to append a link erase
// history below a signed checkpoint without a trace.
func (v *verifier) startsAtFloor(from int64) (int64, bool) {
	if v.prune == nil || from > v.prune.mark.Floor {
		return from, false
	}
	if !v.pruning {
		v.rep.add(Problem{Kind: KindSignedCheckpoint, Seq: v.prune.seq,
			Detail: fmt.Sprintf("the chain holds a prune link (links below %d removed), "+
				"but audit.prune_below_signed_checkpoint is off, so the removed links "+
				"are reported missing", v.prune.mark.Floor)})
		return from, false
	}
	v.prune.floorStart = true
	v.rep.PrunedBelow, v.rep.FromCheckpoint = v.prune.mark.Floor, v.prune.mark.Checkpoint
	return v.prune.mark.Floor, true
}

// checkMarks checks the checkpoint links the last batch walked.
func (v *verifier) checkMarks(ctx context.Context) error {
	for i := range v.marks {
		if err := v.checkMark(ctx, &v.marks[i]); err != nil {
			return err
		}
	}
	v.marks = v.marks[:0]
	return nil
}

// checkMark checks one checkpoint link: its payload, the link it signs
// (unless pruned), and its signature when the trusted key signed it.
func (v *verifier) checkMark(ctx context.Context, l *Link) error {
	bad := func(format string, args ...any) {
		v.rep.add(Problem{Kind: KindSignedCheckpoint, Seq: l.Seq,
			Detail: fmt.Sprintf(format, args...)})
	}
	cp, err := ParseSignedCheckpoint(l.State)
	if err != nil {
		bad("the checkpoint link is unreadable (%v)", err)
		return nil
	}
	if cp.Chain != v.s.Chain || cp.Seq >= l.Seq {
		bad("the checkpoint link signs %s link %d, not an earlier link of this chain",
			cp.Chain, cp.Seq)
		return nil
	}
	if v.prune == nil || cp.Seq >= v.prune.mark.Floor {
		hash, found, err := LinkHashAt(ctx, v.q, v.s.Chain, cp.Seq)
		if err != nil {
			return err
		}
		if !found || hash != cp.Hash {
			bad("link %d, which this checkpoint signed, is missing or was rewritten", cp.Seq)
			return nil
		}
	}
	v.checkSignature(l.Seq, cp)
	return nil
}

func (v *verifier) checkSignature(linkSeq int64, cp SignedCheckpoint) {
	anchor := v.prune != nil && v.prune.floorStart && linkSeq == v.prune.mark.CheckpointLink
	switch {
	case v.key == nil:
		if anchor {
			v.prune.anchorWhy = "cannot be checked: no trusted key was given"
		}
		return
	case cp.KeyID != v.key.ID:
		if anchor {
			v.prune.anchorWhy = fmt.Sprintf("is signed by key %s, not the trusted key %s",
				cp.KeyID, v.key.ID)
		}
		return // signed under another key (rotated): not this key's to judge
	}
	if err := cp.Verify(*v.key); err != nil {
		v.rep.add(Problem{Kind: KindSignedCheckpoint, Seq: linkSeq,
			Detail: fmt.Sprintf("the checkpoint's signature does not verify under key %s",
				v.key.ID)})
		return
	}
	v.rep.SignedCheckpoints++
	if anchor && cp.Seq == v.prune.mark.Checkpoint {
		v.prune.anchorOK = true
	}
}

// reachAnchor carries a window that starts at a prune's floor and ends
// before its anchoring checkpoint link on to that link, checking only the
// chain itself (positions, predecessors, hashes), so the window rests on
// the checkpoint's signature too. A break leaves the anchor unverified,
// which checkPruneAnchor reports.
func (v *verifier) reachAnchor(ctx context.Context, to int64) error {
	p := v.prune
	if p == nil || !p.floorStart || p.anchorOK || to >= p.mark.CheckpointLink {
		return nil
	}
	for next := to + 1; next <= p.mark.CheckpointLink; {
		links, err := LinksInRange(ctx, v.q, v.s.Chain, next, p.mark.CheckpointLink,
			linkBatch)
		if err != nil || len(links) == 0 {
			return err
		}
		for i := range links {
			l := &links[i]
			if l.Seq != next || (v.prevKnown && l.PrevHash != v.prev) ||
				LinkHash(*l) != l.Hash {
				return nil
			}
			v.prev, v.prevKnown, next = l.Hash, true, l.Seq+1
		}
		if last := links[len(links)-1]; last.Seq == p.mark.CheckpointLink &&
			last.Op == OpCheckpoint {
			v.marks = append(v.marks, last)
		}
	}
	return v.checkMarks(ctx)
}

// checkPruneAnchor reports a verification that starts at a prune's floor
// without its anchoring checkpoint verified: nothing then vouches for the
// links it starts from.
func (v *verifier) checkPruneAnchor() {
	p := v.prune
	if p == nil || !p.floorStart || p.anchorOK {
		return
	}
	why := p.anchorWhy
	if why == "" {
		why = "was not verified (missing, rewritten, or outside the window)"
	}
	v.rep.add(Problem{Kind: KindSignedCheckpoint, Seq: p.mark.CheckpointLink,
		Detail: fmt.Sprintf("links below %d were pruned on the strength of the checkpoint "+
			"link %d, which %s", p.mark.Floor, p.mark.CheckpointLink, why)})
}
