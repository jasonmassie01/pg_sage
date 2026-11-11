package auditchain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxProblems caps a report's problem list; ProblemsTruncated says so.
const MaxProblems = 100

const (
	linkBatch = 2000
	rowBatch  = 500
)

// Problem kinds.
const (
	KindGap             = "gap"               // a position is missing
	KindPrevMismatch    = "prev_mismatch"     // a link does not follow its predecessor
	KindHashMismatch    = "hash_mismatch"     // a link's content no longer hashes to it
	KindRowMissing      = "row_missing"       // a row vanished without a delete link
	KindRowEdited       = "row_edited"        // a sealed column changed
	KindStateEdited     = "state_edited"      // state differs from the row's last link
	KindRowLinkMismatch = "row_link_mismatch" // the row's own chain columns disagree
	KindUnchainedRow    = "unchained_row"     // a row written with the triggers bypassed
	KindOrphanRow       = "orphan_row"        // a row's own link is no longer in the chain
	KindUnknownVersion  = "unknown_version"   // a link's payload version is not the spec's
	// KindCheckpointMismatch: the link a checkpoint vouched for is gone,
	// rewritten, or beyond the head.
	KindCheckpointMismatch = "checkpoint_mismatch"
)

// ErrNotInstalled means the chain's table has no chain (yet).
var ErrNotInstalled = errors.New("auditchain: chain not installed")

// Window bounds a verification by position; zero values are open.
// Anchor, when set, is a checkpoint whose link must still carry its hash.
// Key, when set, checks the signatures of the checkpoint links it signed;
// a verification that starts at a prune's floor needs it. Pruning says
// pruning is enabled (audit.prune_below_signed_checkpoint): only then may
// a verification start at a prune link's floor. Unset, the pruned links
// are reported missing, since a prune link the operator never enabled
// proves nothing.
type Window struct {
	FromSeq int64       `json:"from_seq"`
	ToSeq   int64       `json:"to_seq"`
	Anchor  *Checkpoint `json:"-"`
	Key     *TrustedKey `json:"-"`
	Pruning bool        `json:"-"`
}

// Problem is one integrity failure.
type Problem struct {
	Kind   string `json:"kind"`
	Seq    int64  `json:"seq,omitempty"`
	RowID  int64  `json:"row_id,omitempty"`
	Detail string `json:"detail"`
}

// Report is the outcome of one verification.
type Report struct {
	Chain         string `json:"chain"`
	Mode          string `json:"mode"`
	CheckpointSeq int64  `json:"checkpoint_seq,omitempty"`
	// FromCheckpoint is the signed checkpoint a verification of a pruned
	// chain starts from, and PrunedBelow the lowest link the chain keeps.
	FromCheckpoint    int64     `json:"from_checkpoint,omitempty"`
	PrunedBelow       int64     `json:"pruned_below,omitempty"`
	SignedCheckpoints int       `json:"signed_checkpoints,omitempty"`
	FromSeq           int64     `json:"from_seq"`
	ToSeq             int64     `json:"to_seq"`
	Links             int       `json:"links"`
	RowsChecked       int       `json:"rows_checked"`
	Deleted           int       `json:"deleted"`
	Truncations       int       `json:"truncations"`
	LegacyRows        int64     `json:"legacy_rows"`
	HeadSeq           int64     `json:"head_seq"`
	HeadHash          string    `json:"head_hash"`
	Problems          []Problem `json:"problems"`
	ProblemsTruncated bool      `json:"problems_truncated"`
	VerifiedAt        time.Time `json:"verified_at"`
}

// OK reports a verification without problems.
func (r Report) OK() bool { return len(r.Problems) == 0 && !r.ProblemsTruncated }

func (r *Report) add(p Problem) {
	if len(r.Problems) >= MaxProblems {
		r.ProblemsTruncated = true
		return
	}
	r.Problems = append(r.Problems, p)
}

// rowLinks is what the window says about one row.
type rowLinks struct {
	insert  *Link
	sealed  map[string]int64 // sealed hash -> first seq carrying it
	deleted bool
	latest  *latestLink // the last insert or state link read
}

type verifier struct {
	q         Querier
	s         Spec
	rep       Report
	prev      string
	prevKnown bool
	rows      map[int64]*rowLinks
	unknown   map[int64]bool // rows with a link of another payload version
	legacy    int64
	// stream: a full pass keeps no per-row state in the walk; rows are
	// checked by an ordered pass over the link index (stream.go).
	stream  bool
	anchor  *anchor
	key     *TrustedKey
	pruning bool // a prune link's floor may start the verification
	prune   *pruned
	marks   []Link // checkpoint links of the batch being walked
}

// Verify checks chain s within w: every position present, every link
// following its predecessor and hashing to itself, and every row the links
// name matching them (sealed columns, current state, its own link). It
// also finds rows written behind the triggers. A full verification holds
// only the previous hash and counters while it walks, whatever the
// chain's length; a window's memory follows the rows it names.
func Verify(ctx context.Context, q Querier, s Spec, w Window) (Report, error) {
	if err := s.validate(); err != nil {
		return Report{}, fmt.Errorf("auditchain: invalid spec: %w", err)
	}
	if q == nil {
		return Report{}, fmt.Errorf("auditchain: %s: no database", s.Chain)
	}
	if w.FromSeq < 0 || w.ToSeq < 0 || (w.ToSeq > 0 && w.FromSeq > w.ToSeq) {
		return Report{}, fmt.Errorf("auditchain: %s: invalid window %d-%d", s.Chain,
			w.FromSeq, w.ToSeq)
	}
	v := &verifier{q: q, s: s, rows: map[int64]*rowLinks{}, unknown: map[int64]bool{},
		rep: Report{Chain: s.Chain, VerifiedAt: time.Now().UTC()}, key: w.Key,
		pruning: w.Pruning}
	if err := v.run(ctx, w); err != nil {
		return Report{}, fmt.Errorf("auditchain: verify %s: %w", s.Chain, err)
	}
	if v.rep.Problems == nil {
		v.rep.Problems = []Problem{}
	}
	return v.rep, nil
}

func (v *verifier) run(ctx context.Context, w Window) error {
	if err := v.readHead(ctx); err != nil {
		return err
	}
	head := v.rep.HeadSeq
	from, floorStart := v.startsAtFloor(max(w.FromSeq, 1))
	to := w.ToSeq
	if to == 0 || to > head {
		to = head
	}
	v.rep.FromSeq, v.rep.ToSeq = from, to
	full := (from == 1 || floorStart) && to == head
	v.rep.Mode = verifyMode(full, w.Anchor, from, to == head)
	if err := v.checkAnchor(ctx, w.Anchor); err != nil {
		return err
	}
	if from <= to {
		if err := v.checkRange(ctx, from, to, full); err != nil {
			return err
		}
		if err := v.reachAnchor(ctx, to); err != nil {
			return err
		}
		v.checkPruneAnchor()
	}
	if err := v.findUnchained(ctx, full); err != nil {
		return err
	}
	if full {
		if err := v.findOrphans(ctx); err != nil {
			return err
		}
	}
	if from == 1 || floorStart {
		return v.countLegacy(ctx)
	}
	return nil
}

// readHead reads the install boundary and the chain's head.
func (v *verifier) readHead(ctx context.Context) error {
	err := v.q.QueryRow(ctx, `/* pg_sage audit_chain v1 */ SELECT legacy_max_id
		FROM sage.audit_chain_meta WHERE chain = $1`, v.s.Chain).Scan(&v.legacy)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotInstalled
	}
	if err != nil {
		return fmt.Errorf("read install boundary: %w", err)
	}
	head, hash, err := Head(ctx, v.q, v.s.Chain)
	if err != nil {
		return err
	}
	v.rep.HeadSeq, v.rep.HeadHash = head, hash
	return v.readPrune(ctx)
}

func verifyMode(full bool, cp *Checkpoint, from int64, toHead bool) string {
	switch {
	case full:
		return ModeFull
	case cp != nil && from == cp.Seq+1 && toHead:
		return ModeIncremental
	}
	return ModeWindow
}

// checkRange walks the links from-to, then checks the rows they name:
// in full by the ordered row pass, otherwise from what the walk tracked.
func (v *verifier) checkRange(ctx context.Context, from, to int64, full bool) error {
	v.stream = full
	if err := v.walk(ctx, from, to); err != nil {
		return err
	}
	if full {
		return v.checkAllRows(ctx)
	}
	return v.checkRows(ctx, to == v.rep.HeadSeq)
}

// walk reads the window's links in order and checks the chain itself.
func (v *verifier) walk(ctx context.Context, from, to int64) error {
	if err := v.start(ctx, from); err != nil {
		return err
	}
	expected := from
	for expected <= to {
		links, err := LinksInRange(ctx, v.q, v.s.Chain, expected, to, linkBatch)
		if err != nil {
			return err
		}
		if len(links) == 0 {
			v.rep.add(Problem{Kind: KindGap, Seq: expected,
				Detail: fmt.Sprintf("links %d-%d are missing", expected, to)})
			return nil
		}
		for i := range links {
			expected = v.checkLink(&links[i], expected)
		}
		if err := v.checkMarks(ctx); err != nil {
			return err
		}
	}
	return nil
}

// start sets the hash the window's first link must follow.
func (v *verifier) start(ctx context.Context, from int64) error {
	if from == 1 {
		v.prev, v.prevKnown = Genesis, true
		return nil
	}
	if v.prune != nil && v.prune.floorStart {
		v.prevKnown = false // pruned: the checkpoint link vouches for the floor
		return nil
	}
	if a := v.anchor; a != nil && a.seq == from-1 {
		v.prev, v.prevKnown = a.hash, a.found // checkAnchor reported a mismatch
		return nil
	}
	hash, found, err := LinkHashAt(ctx, v.q, v.s.Chain, from-1)
	if err != nil {
		return err
	}
	if !found {
		v.rep.add(Problem{Kind: KindGap, Seq: from - 1,
			Detail: "the link before the window is missing"})
		return nil
	}
	v.prev, v.prevKnown = hash, true
	return nil
}

// checkLink checks one link and returns the next expected position.
func (v *verifier) checkLink(l *Link, expected int64) int64 {
	v.rep.Links++
	if l.Seq != expected {
		v.rep.add(Problem{Kind: KindGap, Seq: expected,
			Detail: fmt.Sprintf("links %d-%d are missing", expected, l.Seq-1)})
		v.prevKnown = false
	}
	if v.prevKnown && l.PrevHash != v.prev {
		v.rep.add(Problem{Kind: KindPrevMismatch, Seq: l.Seq, RowID: l.RowID,
			Detail: "the link does not follow its predecessor (removed or reordered)"})
	}
	if LinkHash(*l) != l.Hash {
		v.rep.add(Problem{Kind: KindHashMismatch, Seq: l.Seq, RowID: l.RowID,
			Detail: "the link's content was changed after it was written"})
	}
	v.prev, v.prevKnown = l.Hash, true
	switch l.Op {
	case "T":
		v.rep.Truncations++
		return l.Seq + 1
	case OpCheckpoint:
		v.marks = append(v.marks, *l)
		return l.Seq + 1
	case OpPrune:
		return l.Seq + 1
	}
	if l.V != v.s.Version {
		v.rep.add(Problem{Kind: KindUnknownVersion, Seq: l.Seq, RowID: l.RowID,
			Detail: fmt.Sprintf("the link has payload version %d; the %s spec is version "+
				"%d, so its row is not checked", l.V, v.s.Chain, v.s.Version)})
		if !v.stream {
			v.unknown[l.RowID] = true
		}
		return l.Seq + 1
	}
	if l.Op == "D" {
		v.rep.Deleted++
	}
	if !v.stream {
		v.track(l)
	}
	return l.Seq + 1
}

// track records what a window link says about its row.
func (v *verifier) track(l *Link) {
	r := v.rows[l.RowID]
	if r == nil {
		r = &rowLinks{sealed: map[string]int64{}}
		v.rows[l.RowID] = r
	}
	r.note(l)
}

// note adds one row link (never a truncate) to r.
func (r *rowLinks) note(l *Link) {
	if _, seen := r.sealed[l.SealedHash]; !seen {
		r.sealed[l.SealedHash] = l.Seq
	}
	switch l.Op {
	case "I":
		copied := *l
		r.insert = &copied
		r.latest = &latestLink{seq: l.Seq, state: l.State}
	case "U":
		r.latest = &latestLink{seq: l.Seq, state: l.State}
	case "D":
		r.deleted = true
	}
}
