package benchsig

import (
	"errors"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"github.com/sigstore/sigstore-go/pkg/testing/data"
)

// Roadmap 1.1 (2026-10-03): the release workflow signs every
// PGIncidentBench report keyless (Sigstore, GitHub OIDC); the sidecar
// verifies the bundle offline against the embedded Sigstore trusted root
// and accepts only the pg_sage CI workflow of the pg_sage repository, on
// master or a v* tag. The virtual Sigstore below stands in for Fulcio,
// Rekor and the CT log, so valid, tampered and foreign signatures are
// real cryptography, not mocks.

const (
	ciTag    = "https://github.com/jasonmassie01/pg_sage/.github/workflows/ci.yml@refs/tags/v1.8.5"
	ciMaster = "https://github.com/jasonmassie01/pg_sage/.github/workflows/ci.yml@refs/heads/master"
)

var report = []byte(`{"schema":"pg_sage.pgincidentbench.v1","pg_sage_version":"1.8.5"}`)

func virtualSigstore(t *testing.T) *ca.VirtualSigstore {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

// releaseVerifier is the release verifier over vs without the SCT
// requirement: the virtual Sigstore cannot issue SCTs (see
// TestReleaseVerifierRequiresAnSCT for the requirement itself).
func releaseVerifier(t *testing.T, vs *ca.VirtualSigstore) *Verifier {
	t.Helper()
	v, err := newVerifier(vs, ReleaseIdentity(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyAcceptsTheReleaseWorkflowSignature(t *testing.T) {
	vs := virtualSigstore(t)
	v := releaseVerifier(t, vs)
	for _, san := range []string{ciTag, ciMaster} {
		entity, err := vs.Sign(san, GitHubIssuer, report)
		if err != nil {
			t.Fatal(err)
		}
		sig, err := v.VerifyEntity(report, entity)
		if err != nil {
			t.Fatalf("%s: valid signature refused: %v", san, err)
		}
		if sig.Identity != san || sig.Issuer != GitHubIssuer || sig.SignedAt.IsZero() {
			t.Fatalf("%s: signature = %+v", san, sig)
		}
	}
}

func TestVerifyRefusesATamperedReport(t *testing.T) {
	vs := virtualSigstore(t)
	entity, err := vs.Sign(ciTag, GitHubIssuer, report)
	if err != nil {
		t.Fatal(err)
	}
	v := releaseVerifier(t, vs)
	tampered := []byte(strings.Replace(string(report), "1.8.5", "1.8.6", 1))
	for name, body := range map[string][]byte{"edited": tampered,
		"truncated": report[:len(report)-1], "appended": append(append([]byte{}, report...),
			'\n')} {
		if _, err := v.VerifyEntity(body, entity); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("%s report: err = %v, want ErrInvalidSignature", name, err)
		}
	}
}

func TestVerifyRefusesOtherSigners(t *testing.T) {
	vs := virtualSigstore(t)
	v := releaseVerifier(t, vs)
	cases := map[string]struct{ san, issuer string }{
		"a fork":            {strings.Replace(ciTag, "jasonmassie01", "attacker", 1), GitHubIssuer},
		"a pull request":    {strings.Replace(ciMaster, "heads/master", "pull/7/merge", 1), GitHubIssuer},
		"a feature branch":  {strings.Replace(ciMaster, "master", "feature/x", 1), GitHubIssuer},
		"another workflow":  {strings.Replace(ciTag, "ci.yml", "docs.yml", 1), GitHubIssuer},
		"a tag without v":   {strings.Replace(ciTag, "tags/v1.8.5", "tags/1.8.5", 1), GitHubIssuer},
		"a suffix":          {ciTag + "/evil", GitHubIssuer},
		"a prefix":          {"x" + ciTag, GitHubIssuer},
		"another issuer":    {ciTag, "https://accounts.google.com"},
		"a person's e-mail": {"maintainer@example.com", "https://github.com/login/oauth"},
	}
	for name, c := range cases {
		entity, err := vs.Sign(c.san, c.issuer, report)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.VerifyEntity(report, entity); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("%s (%s): err = %v, want ErrInvalidSignature", name, c.san, err)
		}
	}
}

// The sidecar's verifier requires an SCT from a trusted CT log in the
// signing certificate (as cosign verify-blob does): a certificate
// without one is refused even when everything else verifies.
func TestReleaseVerifierRequiresAnSCT(t *testing.T) {
	vs := virtualSigstore(t)
	entity, err := vs.Sign(ciTag, GitHubIssuer, report)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(vs, ReleaseIdentity())
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.VerifyEntity(report, entity)
	if !errors.Is(err, ErrInvalidSignature) ||
		!strings.Contains(err.Error(), "certificate timestamp") {
		t.Fatalf("err = %v, want an SCT refusal", err)
	}
}

// A signature from another Sigstore (a private Fulcio and Rekor) does not
// chain to the trusted root and is refused even with the right identity.
func TestVerifyRefusesAnUntrustedSigstore(t *testing.T) {
	trusted, other := virtualSigstore(t), virtualSigstore(t)
	entity, err := other.Sign(ciTag, GitHubIssuer, report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := releaseVerifier(t, trusted).VerifyEntity(report, entity); !errors.Is(err,
		ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
}

func TestVerifyRefusesEmptyInput(t *testing.T) {
	vs := virtualSigstore(t)
	v := releaseVerifier(t, vs)
	entity, err := vs.Sign(ciTag, GitHubIssuer, report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyEntity(nil, entity); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("empty report: err = %v", err)
	}
	if _, err := v.VerifyEntity(report, nil); !errors.Is(err, ErrMalformedBundle) {
		t.Errorf("nil entity: err = %v", err)
	}
	var nilVerifier *Verifier
	if _, err := nilVerifier.Verify(report, []byte(`{}`)); err == nil {
		t.Error("a nil verifier verified something")
	}
}

func TestVerifyRefusesMalformedBundles(t *testing.T) {
	v := releaseVerifier(t, virtualSigstore(t))
	for name, raw := range map[string]string{
		"empty":          "",
		"not JSON":       "not a bundle",
		"empty object":   "{}",
		"unknown media":  `{"mediaType":"application/x-unknown"}`,
		"truncated JSON": `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json",`,
	} {
		_, err := v.Verify(report, []byte(raw))
		if !errors.Is(err, ErrMalformedBundle) {
			t.Errorf("%s: err = %v, want ErrMalformedBundle", name, err)
		}
		if errors.Is(err, ErrInvalidSignature) {
			t.Errorf("%s: a malformed bundle must be distinguishable from a bad signature",
				name)
		}
	}
}

// A real, well-formed public-good bundle (an npm provenance attestation)
// parses, reaches verification and is refused: it is a DSSE attestation,
// not a signature of this report, by another signer.
func TestVerifyRefusesARealBundleOfAnotherArtifact(t *testing.T) {
	b := data.Bundle(t, "sigstore.js@2.0.0-provenance.sigstore.json")
	raw, err := b.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(data.TrustedRoot(t, "public-good.json"), ReleaseIdentity())
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(report, raw)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("err = %v, want ErrInvalidSignature", err)
	}
}

func TestNewVerifierRefusesIncompleteConfiguration(t *testing.T) {
	vs := virtualSigstore(t)
	if _, err := NewVerifier(nil, ReleaseIdentity()); err == nil {
		t.Error("no trusted material accepted")
	}
	if _, err := NewVerifier(vs, Identity{SANPattern: "^x$"}); err == nil {
		t.Error("an empty issuer accepted")
	}
	if _, err := NewVerifier(vs, Identity{Issuer: GitHubIssuer}); err == nil {
		t.Error("an empty identity pattern accepted (it would match any signer)")
	}
	if _, err := NewVerifier(vs, Identity{Issuer: GitHubIssuer, SANPattern: "(["}); err == nil {
		t.Error("an invalid pattern accepted")
	}
	if _, err := NewVerifier(vs, Identity{Issuer: GitHubIssuer, SANPattern: "x"}); err == nil {
		t.Error("an unanchored pattern accepted (it would match a suffix)")
	}
}

// The embedded trusted root (refreshed from Sigstore's TUF repository by
// the release workflow) parses and holds a certificate authority, a
// transparency log and a CT log, so offline verification can work.
func TestEmbeddedTrustedRootIsUsable(t *testing.T) {
	tr, err := ParseTrustedRoot(EmbeddedTrustedRoot())
	if err != nil {
		t.Fatalf("embedded trusted root: %v", err)
	}
	if len(tr.FulcioCertificateAuthorities()) == 0 || len(tr.RekorLogs()) == 0 ||
		len(tr.CTLogs()) == 0 {
		t.Fatalf("embedded root lacks material: %d CAs, %d logs, %d CT logs",
			len(tr.FulcioCertificateAuthorities()), len(tr.RekorLogs()), len(tr.CTLogs()))
	}
	if _, err := NewReleaseVerifier(); err != nil {
		t.Fatalf("release verifier: %v", err)
	}
	for name, raw := range map[string]string{"empty": "", "garbage": "[]",
		"no material": `{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.1"}`} {
		if _, err := ParseTrustedRoot([]byte(raw)); err == nil {
			t.Errorf("%s trusted root accepted", name)
		}
	}
}

// The sidecar verifies from every database runtime's ingest loop at
// once; a verifier is shared and must be safe for concurrent use.
func TestVerifierIsSafeForConcurrentUse(t *testing.T) {
	vs := virtualSigstore(t)
	v := releaseVerifier(t, vs)
	entity, err := vs.Sign(ciTag, GitHubIssuer, report)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 16)
	for i := 0; i < cap(errs); i++ {
		go func(i int) {
			body := report
			if i%2 == 1 {
				body = []byte(strings.Replace(string(report), "1.8.5", "9.9.9", 1))
			}
			_, err := v.VerifyEntity(body, entity)
			if i%2 == 0 && err != nil {
				errs <- err
				return
			}
			if i%2 == 1 && !errors.Is(err, ErrInvalidSignature) {
				errs <- errors.New("tampered report verified under concurrency")
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
