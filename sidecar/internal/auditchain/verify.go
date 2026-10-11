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
)

// ErrNotInstalled means the chain's table has no chain (yet).
var ErrNotInstalled = errors.New("auditchain: chain not installed")

// Window bounds a verification by position; zero values are open.
type Window struct {
	FromSeq int64 `json:"from_seq"`
	ToSeq   int64 `json:"to_seq"`
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
	Chain             string    `json:"chain"`
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
}

type verifier struct {
	q         Querier
	s         Spec
	rep       Report
	prev      string
	prevKnown bool
	rows      map[int64]*rowLinks
	legacy    int64
}

// Verify checks chain s within w: every position present, every link
// following its predecessor and hashing to itself, and every row the links
// name matching them (sealed columns, current state, its own link). It
// also finds rows written behind the triggers.
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
	v := &verifier{q: q, s: s, rows: map[int64]*rowLinks{},
		rep: Report{Chain: s.Chain, VerifiedAt: time.Now().UTC()}}
	if err := v.run(ctx, w); err != nil {
		return Report{}, fmt.Errorf("auditchain: verify %s: %w", s.Chain, err)
	}
	if v.rep.Problems == nil {
		v.rep.Problems = []Problem{}
	}
	return v.rep, nil
}

func (v *verifier) run(ctx context.Context, w Window) error {
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
	from, to := max(w.FromSeq, 1), w.ToSeq
	if to == 0 || to > head {
		to = head
	}
	v.rep.FromSeq, v.rep.ToSeq = from, to
	full := from == 1 && to == head
	if from <= to {
		if err := v.walk(ctx, from, to); err != nil {
			return err
		}
		if err := v.checkRows(ctx); err != nil {
			return err
		}
	}
	if err := v.findUnchained(ctx, full); err != nil {
		return err
	}
	if from == 1 {
		return v.countLegacy(ctx)
	}
	return nil
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
	}
	return nil
}

// start sets the hash the window's first link must follow.
func (v *verifier) start(ctx context.Context, from int64) error {
	if from == 1 {
		v.prev, v.prevKnown = Genesis, true
		return nil
	}
	err := v.q.QueryRow(ctx, linkAtSQL, v.s.Chain, from-1).Scan(&v.prev)
	if errors.Is(err, pgx.ErrNoRows) {
		v.rep.add(Problem{Kind: KindGap, Seq: from - 1,
			Detail: "the link before the window is missing"})
		return nil
	}
	if err != nil {
		return fmt.Errorf("read link %d: %w", from-1, err)
	}
	v.prevKnown = true
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
	v.track(l)
	return l.Seq + 1
}

func (v *verifier) track(l *Link) {
	switch l.Op {
	case "T":
		v.rep.Truncations++
		return
	case "D":
		v.rep.Deleted++
	}
	r := v.rows[l.RowID]
	if r == nil {
		r = &rowLinks{sealed: map[string]int64{}}
		v.rows[l.RowID] = r
	}
	if _, seen := r.sealed[l.SealedHash]; !seen {
		r.sealed[l.SealedHash] = l.Seq
	}
	switch l.Op {
	case "I":
		copied := *l
		r.insert = &copied
	case "D":
		r.deleted = true
	}
}
