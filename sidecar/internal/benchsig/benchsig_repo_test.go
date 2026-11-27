package benchsig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// benches-10 (2026-11-08): the release identity pins the GitHub
// repository and owner by their immutable numeric IDs (Fulcio extensions
// 1.3.6.1.4.1.57264.1.15 and .17), not only by name. A repository that is
// renamed or transferred, and whose name is then registered again, gets
// new IDs, so a ci.yml there signs reports no shipped sidecar accepts.

// The IDs Fulcio put in the v2.4.0 release certificates (testdata).
const (
	shippedRepositoryID = "1185895123"
	shippedOwnerID      = "143746527"
)

// releaseSummary is a certificate of the release workflow with the given
// repository and owner IDs.
func releaseSummary(san, repoID, ownerID string) certificate.Summary {
	return certificate.Summary{SubjectAlternativeName: san,
		Extensions: certificate.Extensions{Issuer: GitHubIssuer,
			SourceRepositoryIdentifier:      repoID,
			SourceRepositoryOwnerIdentifier: ownerID}}
}

func pinnedReleaseVerifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(virtualSigstore(t), ReleaseIdentity())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReleaseIdentityRefusesAnotherRepositoryWithTheSameName(t *testing.T) {
	v := pinnedReleaseVerifier(t)
	cases := map[string]struct{ repo, owner string }{
		"another repository ID":        {"999999999", shippedOwnerID},
		"another owner ID":             {shippedRepositoryID, "1"},
		"both IDs swapped":             {shippedOwnerID, shippedRepositoryID},
		"no repository ID":             {"", shippedOwnerID},
		"no owner ID":                  {shippedRepositoryID, ""},
		"no repository extensions":     {"", ""},
		"a leading zero":               {"0" + shippedRepositoryID, shippedOwnerID},
		"a trailing space":             {shippedRepositoryID + " ", shippedOwnerID},
		"the repository ID one higher": {"1185895124", shippedOwnerID},
	}
	for name, c := range cases {
		for _, san := range []string{ciTag, ciMaster} {
			err := v.identity.Verify(releaseSummary(san, c.repo, c.owner))
			if err == nil {
				t.Errorf("%s (%s): a same-name repository with repository ID %q and "+
					"owner ID %q was accepted", name, san, c.repo, c.owner)
			}
		}
	}
}

func TestReleaseIdentityAcceptsThePinnedRepository(t *testing.T) {
	v := pinnedReleaseVerifier(t)
	for _, san := range []string{ciTag, ciMaster} {
		s := releaseSummary(san, shippedRepositoryID, shippedOwnerID)
		if err := v.identity.Verify(s); err != nil {
			t.Errorf("%s: the pinned repository was refused: %v", san, err)
		}
	}
}

// The right IDs do not widen the name check: a fork, another workflow or
// another issuer is refused even when the IDs match.
func TestReleaseIdentityStillChecksTheWorkflowWithTheRightIDs(t *testing.T) {
	v := pinnedReleaseVerifier(t)
	for name, san := range map[string]string{
		"a fork":           strings.Replace(ciTag, "jasonmassie01", "attacker", 1),
		"a pull request":   strings.Replace(ciMaster, "heads/master", "pull/7/merge", 1),
		"another workflow": strings.Replace(ciTag, "ci.yml", "docs.yml", 1),
	} {
		s := releaseSummary(san, shippedRepositoryID, shippedOwnerID)
		if err := v.identity.Verify(s); err == nil {
			t.Errorf("%s accepted with the pinned IDs", name)
		}
	}
	s := releaseSummary(ciTag, shippedRepositoryID, shippedOwnerID)
	s.Issuer = "https://accounts.google.com"
	if err := v.identity.Verify(s); err == nil {
		t.Error("another issuer accepted with the pinned IDs")
	}
}

// Every virtual-Sigstore certificate lacks the repository extensions, as
// a certificate from a same-name repository would carry other IDs: the
// release verifier refuses it although SAN, issuer and signature verify.
func TestReleaseVerifierRefusesACertificateWithoutRepositoryIDs(t *testing.T) {
	vs := virtualSigstore(t)
	entity, err := vs.Sign(ciTag, GitHubIssuer, report)
	if err != nil {
		t.Fatal(err)
	}
	v, err := newVerifier(vs, ReleaseIdentity(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.VerifyEntity(report, entity)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
	if errors.Is(err, ErrMalformedBundle) {
		t.Fatalf("err = %v: an unpinned certificate is a bad signature, not a malformed "+
			"bundle", err)
	}
	if !strings.Contains(err.Error(), "SourceRepositoryIdentifier") {
		t.Fatalf("err = %v, want it to name the repository ID", err)
	}
}

// shippedSummary is the leaf certificate of the core report shipped in
// the v2.4.0 image, signed keyless by the real release workflow.
func shippedSummary(t *testing.T) certificate.Summary {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "v2.4.0-core.sigstore.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &bundle.Bundle{}
	if err := b.UnmarshalJSON(raw); err != nil {
		t.Fatalf("shipped bundle: %v", err)
	}
	vc, err := b.VerificationContent()
	if err != nil || vc.Certificate() == nil {
		t.Fatalf("shipped bundle has no certificate: %v", err)
	}
	s, err := certificate.SummarizeCertificate(vc.Certificate())
	if err != nil {
		t.Fatalf("shipped certificate: %v", err)
	}
	return s
}

// A real Fulcio certificate of a shipped release carries the pinned IDs,
// so reports signed before the pin keep verifying; the same certificate
// with another repository ID is refused.
func TestShippedReleaseCertificateMatchesThePinnedIdentity(t *testing.T) {
	s := shippedSummary(t)
	if s.SourceRepositoryIdentifier != shippedRepositoryID ||
		s.SourceRepositoryOwnerIdentifier != shippedOwnerID {
		t.Fatalf("shipped certificate names repository %q owner %q",
			s.SourceRepositoryIdentifier, s.SourceRepositoryOwnerIdentifier)
	}
	v := pinnedReleaseVerifier(t)
	if err := v.identity.Verify(s); err != nil {
		t.Fatalf("the v2.4.0 release certificate was refused: %v", err)
	}
	s.SourceRepositoryIdentifier = "999999999"
	if err := v.identity.Verify(s); err == nil {
		t.Fatal("the v2.4.0 certificate with another repository ID was accepted")
	}
}

// The shipped bundle verifies in full (Fulcio chain, SCT, transparency
// log and timestamp against the embedded trusted root) through the
// production verifier with the pins, so the testdata is a real release
// signature and reports signed before the pin keep verifying. The report
// itself is not committed: the bundle's signed digest stands in for it.
func TestShippedReleaseBundleVerifiesWithThePinnedReleaseVerifier(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "v2.4.0-core.sigstore.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &bundle.Bundle{}
	if err := b.UnmarshalJSON(raw); err != nil {
		t.Fatalf("shipped bundle: %v", err)
	}
	digest := b.GetMessageSignature().GetMessageDigest().GetDigest()
	if len(digest) != 32 {
		t.Fatalf("shipped bundle signs a %d-byte digest, want SHA-256", len(digest))
	}
	v, err := NewReleaseVerifier()
	if err != nil {
		t.Fatal(err)
	}
	policy := func(id verify.CertificateIdentity) verify.PolicyBuilder {
		return verify.NewPolicy(verify.WithArtifactDigest("sha256", digest),
			verify.WithCertificateIdentity(id))
	}
	res, err := v.verifier.Verify(b, policy(v.identity))
	if err != nil {
		t.Fatalf("the v2.4.0 release bundle was refused by the pinned verifier: %v", err)
	}
	c := res.Signature.Certificate
	if c.SourceRepositoryIdentifier != shippedRepositoryID ||
		c.SourceRepositoryOwnerIdentifier != shippedOwnerID {
		t.Fatalf("verified certificate names repository %q owner %q",
			c.SourceRepositoryIdentifier, c.SourceRepositoryOwnerIdentifier)
	}
	other := ReleaseIdentity()
	other.OwnerID = "143746528"
	ov, err := NewVerifier(virtualSigstore(t), other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.verifier.Verify(b, policy(ov.identity)); err == nil ||
		!strings.Contains(err.Error(), "SourceRepositoryOwnerIdentifier") {
		t.Fatalf("err = %v, want the real bundle refused for another owner ID", err)
	}
}

func TestReleaseIdentityPinsTheShippedIDs(t *testing.T) {
	id := ReleaseIdentity()
	if id.RepositoryID != shippedRepositoryID || id.OwnerID != shippedOwnerID {
		t.Fatalf("release identity pins repository %q owner %q, want %q and %q",
			id.RepositoryID, id.OwnerID, shippedRepositoryID, shippedOwnerID)
	}
}

// NewVerifier refuses an identity without both pins: an unpinned
// identity would trust any repository registered under the name.
func TestNewVerifierRefusesAnUnpinnedRepository(t *testing.T) {
	vs := virtualSigstore(t)
	bad := map[string]string{"empty": "", "blank": " ", "a name": "pg_sage",
		"negative": "-1185895123", "zero": "0", "a leading zero": "01185895123",
		"a trailing space": "1185895123 ", "hex": "0x46b0a2d3", "a decimal": "1.5"}
	for name, v := range bad {
		id := ReleaseIdentity()
		id.RepositoryID = v
		if _, err := NewVerifier(vs, id); err == nil ||
			!strings.Contains(err.Error(), "repository ID") {
			t.Errorf("repository ID %s (%q): err = %v, want a repository ID refusal",
				name, v, err)
		}
		id = ReleaseIdentity()
		id.OwnerID = v
		if _, err := NewVerifier(vs, id); err == nil ||
			!strings.Contains(err.Error(), "owner ID") {
			t.Errorf("owner ID %s (%q): err = %v, want an owner ID refusal", name, v, err)
		}
	}
	// Boundary: the smallest valid ID is accepted.
	id := ReleaseIdentity()
	id.RepositoryID, id.OwnerID = "1", "1"
	if _, err := NewVerifier(vs, id); err != nil {
		t.Errorf("IDs \"1\" refused: %v", err)
	}
}

// No concurrent-access test here: the pins are immutable fields of the
// shared Verifier, which TestVerifierIsSafeForConcurrentUse exercises.

// Both signing jobs in ci.yml refuse to sign unless the workflow runs in
// the repository and owner ReleaseIdentity pins: the check and the pin
// must name the same IDs, or every master push fails at signing.
func TestSigningJobsCheckThePinnedIDs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read the CI workflow: %v", err)
	}
	ci := strings.ReplaceAll(string(raw), "\r\n", "\n")
	id := ReleaseIdentity()
	for _, job := range []string{"\n  bench-sign:\n", "\n  safety-bench-sign:\n"} {
		name := strings.Trim(job, "\n :")
		start := strings.Index(ci, job)
		if start < 0 {
			t.Fatalf("job %q not in ci.yml", name)
		}
		body := ci[start+len(job):]
		if end := strings.Index(body, "\n  # "); end >= 0 {
			body = body[:end]
		}
		check := strings.Index(body, "github.repository_id")
		sign := strings.Index(body, "cosign sign-blob")
		for _, want := range []string{"github.repository_owner_id",
			`"$REPO_ID" != "` + id.RepositoryID + `"`, `"$OWNER_ID" != "` + id.OwnerID + `"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: no %s in the job", name, want)
			}
		}
		if check < 0 || sign < 0 || check > sign {
			t.Errorf("%s: the repository ID check must come before signing (check at %d, "+
				"sign at %d)", name, check, sign)
		}
	}
}
