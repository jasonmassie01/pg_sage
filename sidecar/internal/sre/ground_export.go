package sre

import "github.com/pg-sage/sidecar/internal/agentloop"

// GroundClaim is the investigator's grounding rule for other users of the
// agent loop (Ask Sage): every number in a claim must appear in the
// evidence it cites. It returns ErrUngroundedNumber (wrapped) otherwise.
func GroundClaim(text string, cited []agentloop.Evidence) error {
	return groundClaim(text, cited)
}
