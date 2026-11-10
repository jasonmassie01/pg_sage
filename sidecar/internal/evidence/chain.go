package evidence

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// chainProof is a chain's evidence: the links, their verification and the
// chain head when the pack was built (an anchor a later pack or the SIEM
// can be compared against), from the database Source names.
type chainProof struct {
	Chain        string             `json:"chain"`
	Source       string             `json:"source,omitempty"`
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
	links, err := auditchain.LinksForRow(ctx, b.db.q, spec.Chain, id)
	if err != nil {
		return fmt.Errorf("evidence: chain of action %d: %w", id, err)
	}
	proof := chainProof{Chain: spec.Chain, Source: b.db.name, Links: links}
	if len(links) == 0 {
		proof.Note = "the action predates the audit chain (no links)"
	} else {
		w := auditchain.Window{FromSeq: links[0].Seq, ToSeq: links[len(links)-1].Seq}
		if err := b.verify(ctx, b.db, spec, w, &proof); err != nil {
			return err
		}
	}
	if err := b.head(ctx, b.db, spec.Chain, &proof); err != nil {
		return err
	}
	return b.add("chain.json", fileMeta{source: b.db.name}, proof)
}

func (b *builder) verify(ctx context.Context, src source, spec auditchain.Spec,
	w auditchain.Window, proof *chainProof) error {
	rep, err := auditchain.Verify(ctx, src.q, spec, w)
	if errors.Is(err, auditchain.ErrNotInstalled) {
		proof.Note = "chain not installed"
		return nil
	}
	if err != nil {
		return fmt.Errorf("evidence: verify %s in %s: %w", spec.Chain, src.describe(), err)
	}
	proof.Verification = &rep
	if !rep.OK() {
		b.pack.Manifest.ChainOK = false
	}
	return nil
}

func (b *builder) head(ctx context.Context, src source, chain string,
	proof *chainProof) error {
	seq, hash, err := auditchain.Head(ctx, src.q, chain)
	if err != nil {
		return fmt.Errorf("evidence: %s: %w", src.describe(), err)
	}
	proof.Head = head{Seq: seq, Hash: hash}
	return nil
}
