package api

import (
	"crypto/ed25519"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/auditjob"
	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/siem"
)

// auditStatusResponse is GET /api/v1/audit/status (audit-12): the last
// scheduled verification of every chain, SIEM delivery health and whether
// evidence packs are signed. OK and VerifiedAt are null until a chain has
// been verified in this process; on a fleet follower they stay null
// (verification runs on the leader).
type auditStatusResponse struct {
	OK                  *bool                       `json:"ok"`
	VerifiedAt          *time.Time                  `json:"verified_at"`
	VerifyIntervalHours int                         `json:"verify_interval_hours"`
	Chains              []auditjob.ChainHealth      `json:"chains"`
	VerifyErrors        []auditjob.VerifyErrorCount `json:"verify_errors"`
	SIEM                auditStatusSIEM             `json:"siem"`
	Evidence            auditStatusEvidence         `json:"evidence"`
}

type auditStatusSIEM struct {
	Enabled bool              `json:"enabled"`
	Sinks   []siem.SinkStatus `json:"sinks"`
}

type auditStatusEvidence struct {
	Signed bool   `json:"signed"`
	KeyID  string `json:"key_id"`
	Source string `json:"source"`
}

// auditStatusHandler serves the registry as recorded; it never verifies
// (GET /api/v1/audit/verify does, on demand).
func auditStatusHandler(d AuditDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var h auditjob.HealthSnapshot
		if d.Health != nil {
			h = d.Health()
		}
		resp := auditStatusResponse{VerifiedAt: h.VerifiedAt,
			VerifyIntervalHours: d.VerifyIntervalHours,
			Chains:              append([]auditjob.ChainHealth{}, h.Chains...),
			VerifyErrors:        append([]auditjob.VerifyErrorCount{}, h.Errors...),
			SIEM: auditStatusSIEM{Enabled: d.SIEMStatus != nil,
				Sinks: []siem.SinkStatus{}},
			Evidence: d.evidenceStatus()}
		if len(resp.Chains) > 0 {
			ok := true
			for _, c := range resp.Chains {
				ok = ok && c.OK
			}
			resp.OK = &ok
		}
		if d.SIEMStatus != nil {
			resp.SIEM.Sinks = append(resp.SIEM.Sinks, d.SIEMStatus()...)
		}
		jsonResponse(w, resp)
	}
}

// evidenceStatus says whether packs are signed, and by which key.
func (d AuditDeps) evidenceStatus() auditStatusEvidence {
	if len(d.EvidenceKey) != ed25519.PrivateKeySize {
		return auditStatusEvidence{}
	}
	pub, _ := d.EvidenceKey.Public().(ed25519.PublicKey)
	source := d.EvidenceKeySource
	if source == "" {
		source = evidence.KeySourceEnv
	}
	return auditStatusEvidence{Signed: true, KeyID: evidence.KeyID(pub), Source: source}
}
