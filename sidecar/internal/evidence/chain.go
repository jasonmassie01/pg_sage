package evidence

import (
	"context"
	"errors"
	"fmt"
	"sort"

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

// actionChain proves the action row in chain.json, as before, and in
// chains.json the action, its decisions and the approvals of the policy
// versions they name (audit-11): the authority the change ran under.
func (b *builder) actionChain(ctx context.Context, id int64) error {
	action, err := b.rowsProof(ctx, b.db, auditchain.ActionLog, []int64{id},
		"the action predates the audit chain (no links)")
	if err != nil {
		return err
	}
	if err := b.add("chain.json", fileMeta{source: b.db.name}, action); err != nil {
		return err
	}
	decisions, err := b.decisionIDs(ctx, id)
	if err != nil {
		return err
	}
	decision := chainProof{Chain: auditchain.DecisionLedger.Chain, Source: b.db.name,
		Note: "the action names no decision"}
	if len(decisions) > 0 {
		decision, err = b.rowsProof(ctx, b.db, auditchain.DecisionLedger, decisions,
			"the decisions predate the audit chain (no links)")
		if err != nil {
			return err
		}
	}
	approvals, err := b.approvalsProof(ctx)
	if err != nil {
		return err
	}
	return b.add("chains.json", fileMeta{}, []chainProof{action, decision, approvals})
}

// rowsProof proves rows ids of spec in src: their links, verified over the
// positions from the first to the last.
func (b *builder) rowsProof(ctx context.Context, src source, spec auditchain.Spec,
	ids []int64, noLinks string) (chainProof, error) {
	proof := chainProof{Chain: spec.Chain, Source: src.name}
	for _, id := range ids {
		links, err := auditchain.LinksForRow(ctx, src.q, spec.Chain, id)
		if err != nil {
			return proof, fmt.Errorf("evidence: chain of %s %d in %s: %w", spec.Chain, id,
				src.describe(), err)
		}
		proof.Links = append(proof.Links, links...)
	}
	sort.Slice(proof.Links, func(i, j int) bool { return proof.Links[i].Seq < proof.Links[j].Seq })
	if len(proof.Links) == 0 {
		proof.Note = noLinks
	} else {
		w := auditchain.Window{FromSeq: proof.Links[0].Seq,
			ToSeq: proof.Links[len(proof.Links)-1].Seq}
		if err := b.verify(ctx, src, spec, w, &proof); err != nil {
			return proof, err
		}
	}
	return proof, b.head(ctx, src, spec.Chain, &proof)
}

// decisionIDs lists the decisions of action id.
func (b *builder) decisionIDs(ctx context.Context, id int64) ([]int64, error) {
	var ids []int64
	err := b.db.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT coalesce(array_agg(t.id
		ORDER BY t.id), '{}') FROM sage.decision t WHERE `+decisionFilter("t"),
		id).Scan(&ids)
	if err != nil {
		return nil, fmt.Errorf("evidence: decisions of action %d: %w", id, err)
	}
	return ids, nil
}

// approvalsProof proves the approvals of the policy versions in
// policy.json, in the database the policy lives in: their links, and a
// verification of the whole approvals chain, so an approval deleted
// behind the chain (which no query can list any more) is still reported.
// Approvals are rare human acts; the chain is small.
func (b *builder) approvalsProof(ctx context.Context) (chainProof, error) {
	spec := auditchain.PolicyApprovals
	proof := chainProof{Chain: spec.Chain, Source: b.control.name}
	if len(b.policyIDs) == 0 {
		proof.Note = "the decisions name no stored policy version"
		return proof, nil
	}
	if b.approvalIDs == nil {
		proof.Note = "table not installed"
		return proof, nil
	}
	for _, id := range b.approvalIDs {
		links, err := auditchain.LinksForRow(ctx, b.control.q, spec.Chain, id)
		if err != nil {
			return proof, fmt.Errorf("evidence: chain of approval %d in %s: %w", id,
				b.control.describe(), err)
		}
		proof.Links = append(proof.Links, links...)
	}
	sort.Slice(proof.Links, func(i, j int) bool { return proof.Links[i].Seq < proof.Links[j].Seq })
	if err := b.verify(ctx, b.control, spec, auditchain.Window{}, &proof); err != nil {
		return proof, err
	}
	return proof, b.head(ctx, b.control, spec.Chain, &proof)
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
