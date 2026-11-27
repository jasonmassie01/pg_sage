package siem

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// markerMapping reports the chain's own marker links (audit-03b): a signed
// checkpoint, which gives the SIEM a signed statement of the chain's hash
// at a position, and a prune, which says which links the database no
// longer keeps. Both carry the chain's class; a prune is worth a look. A
// payload that does not parse is still exported, at medium severity.
func markerMapping(r Record) (mapping, bool) {
	m := mapping{class: chainClass(r.Chain), activity: 99, status: statusSuccess,
		severity: SeverityInformational}
	switch r.Link.Op {
	case auditchain.OpCheckpoint:
		m.activityName = "Audit Checkpoint Signed"
		cp, err := auditchain.ParseSignedCheckpoint(r.Link.State)
		if err != nil {
			m.severity, m.message = SeverityMedium, fmt.Sprintf("%s checkpoint link %d "+
				"has an unreadable payload: %v", r.Chain, r.Link.Seq, err)
			return m, true
		}
		m.message = fmt.Sprintf("%s verified clean through link %d (hash %s), signed by "+
			"key %s", r.Chain, cp.Seq, cp.Hash, cp.KeyID)
	case auditchain.OpPrune:
		m.activityName, m.severity = "Audit Trail Pruned", SeverityMedium
		p, err := auditchain.ParsePruneMark(r.Link.State)
		if err != nil {
			m.message = fmt.Sprintf("%s prune link %d has an unreadable payload: %v",
				r.Chain, r.Link.Seq, err)
			return m, true
		}
		m.message = fmt.Sprintf("%s links below %d pruned (%d links) under signed "+
			"checkpoint %d (checkpoint link %d)", r.Chain, p.Floor, p.Pruned, p.Checkpoint,
			p.CheckpointLink)
	default:
		return mapping{}, false
	}
	return m, true
}
