package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Decision provenance (policy-03). The gate stamps each decision with the
// version and document hash of the standing policy it evaluated, so the
// recorder never reads the policy a second time: a ratification between
// two reads recorded a version the gate did not decide under.

// DocumentFromPolicy parses a stored policy row into the document the gate
// evaluates, stamped with the row's profile, version and the SHA-256 of
// its stored jsonb text. An unparsable document is an error and carries
// no provenance.
func DocumentFromPolicy(stored Policy) (Document, error) {
	doc, err := ParseDocument(stored.Document)
	if err != nil {
		return Document{}, fmt.Errorf("standing policy v%d: %w", stored.Version, err)
	}
	sum := sha256.Sum256(stored.Document)
	doc.Profile = Profile(stored.Profile)
	doc.Version, doc.SHA256 = stored.Version, hex.EncodeToString(sum[:])
	return doc, nil
}

// documentRead is the policy document one evaluation read. Each evaluation
// owns one, so concurrent decisions never see each other's policy.
type documentRead struct {
	version int64
	sha256  string
}

type documentReadKey struct{}

// evaluate decides req and stamps the decision with the policy document
// the evaluation read, or with none (0, "") when it read none.
func (gate *authorizationGate) evaluate(ctx context.Context, req ActionRequest) Decision {
	read := &documentRead{}
	decision := gate.evaluateRequest(context.WithValue(ctx, documentReadKey{}, read), req)
	decision.PolicyVersion, decision.PolicySHA256 = read.version, read.sha256
	return decision
}

// noteDocumentRead records a successful policy read on the evaluation in
// ctx. Reads outside an evaluation (MaintenanceWindowOpen) record nothing.
func noteDocumentRead(ctx context.Context, doc Document) {
	if read, ok := ctx.Value(documentReadKey{}).(*documentRead); ok {
		read.version, read.sha256 = doc.Version, doc.SHA256
	}
}
