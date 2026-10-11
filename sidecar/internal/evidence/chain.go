package evidence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// chainProof is a chain's evidence: the links, their verification and the
// chain head when the pack was built (an anchor a later pack or the SIEM
// can be compared against).
type chainProof struct {
	Chain        string             `json:"chain"`
	Links        []auditchain.Link  `json:"links,omitempty"`
	Verification *auditchain.Report `json:"verification,omitempty"`
	Head         head               `json:"head"`
	Note         string             `json:"note,omitempty"`
}

type head struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// actionChain proves the action row: its links, verified end to end.
func (b *builder) actionChain(ctx context.Context, id int64) error {
	spec := auditchain.ActionLog
	links, err := auditchain.LinksForRow(ctx, b.q, spec.Chain, id)
	if err != nil {
		return fmt.Errorf("evidence: chain of action %d: %w", id, err)
	}
	proof := chainProof{Chain: spec.Chain, Links: links}
	if len(links) == 0 {
		proof.Note = "the action predates the audit chain (no links)"
	} else {
		w := auditchain.Window{FromSeq: links[0].Seq, ToSeq: links[len(links)-1].Seq}
		if err := b.verify(ctx, spec, w, &proof); err != nil {
			return err
		}
	}
	if err := b.head(ctx, spec.Chain, &proof); err != nil {
		return err
	}
	return b.add("chain.json", proof)
}

func (b *builder) verify(ctx context.Context, spec auditchain.Spec, w auditchain.Window,
	proof *chainProof) error {
	rep, err := auditchain.Verify(ctx, b.q, spec, w)
	if errors.Is(err, auditchain.ErrNotInstalled) {
		proof.Note = "chain not installed"
		return nil
	}
	if err != nil {
		return fmt.Errorf("evidence: verify %s: %w", spec.Chain, err)
	}
	proof.Verification = &rep
	if !rep.OK() {
		b.pack.Manifest.ChainOK = false
	}
	return nil
}

func (b *builder) head(ctx context.Context, chain string, proof *chainProof) error {
	seq, hash, err := auditchain.Head(ctx, b.q, chain)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	proof.Head = head{Seq: seq, Hash: hash}
	return nil
}

// windowSources are the audit tables of a window pack and their time
// columns.
var windowSources = []struct {
	file    string
	spec    auditchain.Spec
	timeCol string
}{
	{"actions.json", auditchain.ActionLog, "executed_at"},
	{"auth_audit.json", auditchain.AuthAudit, "created_at"},
	{"config_audit.json", auditchain.ConfigAudit, "changed_at"},
	{"pgaudit.json", auditchain.PGAuditEvents, "logged_at"},
}

// window collects the audit of [from, to): the rows of every audit table
// and decision in it, and each chain verified over the positions they span.
func (b *builder) window(ctx context.Context, from, to time.Time) error {
	where := func(col string) string {
		return fmt.Sprintf("t.%s >= $1 AND t.%s < $2", col, col)
	}
	var proofs []chainProof
	for _, src := range windowSources {
		if err := b.rows(ctx, src.file, src.spec.Table, where(src.timeCol), from,
			to); err != nil {
			return err
		}
		proof, err := b.windowChain(ctx, src.spec, src.timeCol, from, to)
		if err != nil {
			return err
		}
		proofs = append(proofs, proof)
	}
	if err := b.rows(ctx, "decisions.json", "sage.decision", where("created_at"), from,
		to); err != nil {
		return err
	}
	return b.add("chains.json", proofs)
}

// windowChain verifies a chain over the positions of the window's rows.
func (b *builder) windowChain(ctx context.Context, spec auditchain.Spec, timeCol string,
	from, to time.Time) (chainProof, error) {
	proof := chainProof{Chain: spec.Chain}
	var lo, hi *int64
	sql := fmt.Sprintf(`/* pg_sage evidence v1 */ SELECT min(chain_seq), max(chain_seq)
		FROM %s WHERE %s >= $1 AND %s < $2`, spec.Table, timeCol, timeCol)
	if err := b.q.QueryRow(ctx, sql, from, to).Scan(&lo, &hi); err != nil {
		return proof, fmt.Errorf("evidence: %s positions: %w", spec.Chain, err)
	}
	if lo == nil || hi == nil {
		proof.Note = "no chained rows in the window"
	} else if err := b.verify(ctx, spec, auditchain.Window{FromSeq: *lo, ToSeq: *hi},
		&proof); err != nil {
		return proof, err
	}
	return proof, b.head(ctx, spec.Chain, &proof)
}
