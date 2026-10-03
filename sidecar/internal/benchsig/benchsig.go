// Package benchsig verifies the Sigstore signature of a PGIncidentBench
// report (roadmap 1.1). The release workflow signs every report keyless
// (cosign sign-blob with the GitHub Actions OIDC identity); the sidecar
// verifies the bundle offline against the Sigstore trusted root embedded
// in its binary and accepts only the pg_sage CI workflow of the pg_sage
// repository, on master or a v* tag.
package benchsig

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Errors. A malformed bundle is distinguishable from a signature that
// does not verify (a tampered report or another signer).
var (
	ErrMalformedBundle  = errors.New("malformed signature bundle")
	ErrInvalidSignature = errors.New("signature verification failed")
)

// GitHubIssuer is the OIDC issuer of GitHub Actions workflow identities.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

// releaseSANPattern is the pg_sage CI workflow on master or a v* tag.
const releaseSANPattern = `^https://github\.com/jasonmassie01/pg_sage/\.github/workflows/` +
	`ci\.yml@refs/(heads/master|tags/v[0-9][0-9A-Za-z.+-]*)$`

//go:embed trusted_root.json
var trustedRootJSON []byte

// Identity is the signer a report must come from.
type Identity struct {
	// Issuer is the exact OIDC issuer of the signing certificate.
	Issuer string
	// SANPattern is an anchored regexp the certificate's subject
	// alternative name (the workflow identity) must match.
	SANPattern string
}

// ReleaseIdentity is the pg_sage release workflow.
func ReleaseIdentity() Identity {
	return Identity{Issuer: GitHubIssuer, SANPattern: releaseSANPattern}
}

// Signature is what a verified signature proves.
type Signature struct {
	// Identity is the signing workflow (certificate SAN).
	Identity string `json:"identity"`
	Issuer   string `json:"issuer"`
	// Commit is the source repository digest the certificate names (the
	// commit CI built), empty when the certificate carries none.
	Commit string `json:"commit,omitempty"`
	// Ref is the source repository ref the certificate names.
	Ref string `json:"ref,omitempty"`
	// SignedAt is the earliest verified timestamp of the signature.
	SignedAt time.Time `json:"signed_at"`
}

// Verifier verifies report signatures. It holds immutable trusted
// material and is safe for concurrent use.
type Verifier struct {
	verifier *verify.Verifier
	identity verify.CertificateIdentity
}

// NewVerifier verifies against material for signatures of id: a Fulcio
// certificate with an embedded SCT, a transparency-log entry and an
// observer timestamp (log integration time or a TSA timestamp).
func NewVerifier(material root.TrustedMaterial, id Identity) (*Verifier, error) {
	return newVerifier(material, id, 1)
}

// newVerifier is NewVerifier with a CT-log threshold: always 1 in the
// sidecar (Fulcio embeds an SCT in every certificate); the virtual
// Sigstore of the tests cannot issue SCTs.
func newVerifier(material root.TrustedMaterial, id Identity, scts int) (*Verifier,
	error) {
	switch {
	case material == nil:
		return nil, errors.New("benchsig: no trusted material")
	case strings.TrimSpace(id.Issuer) == "":
		return nil, errors.New("benchsig: no certificate issuer")
	case !strings.HasPrefix(id.SANPattern, "^") || !strings.HasSuffix(id.SANPattern, "$"):
		return nil, fmt.Errorf("benchsig: identity pattern %q must be anchored (^...$)",
			id.SANPattern)
	}
	if _, err := regexp.Compile(id.SANPattern); err != nil {
		return nil, fmt.Errorf("benchsig: identity pattern: %w", err)
	}
	certID, err := verify.NewShortCertificateIdentity(id.Issuer, "", "", id.SANPattern)
	if err != nil {
		return nil, fmt.Errorf("benchsig: certificate identity: %w", err)
	}
	opts := []verify.VerifierOption{verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1)}
	if scts > 0 {
		opts = append(opts, verify.WithSignedCertificateTimestamps(scts))
	}
	v, err := verify.NewVerifier(material, opts...)
	if err != nil {
		return nil, fmt.Errorf("benchsig: verifier: %w", err)
	}
	return &Verifier{verifier: v, identity: certID}, nil
}

// EmbeddedTrustedRoot is the Sigstore trusted root built into this
// binary (refreshed from Sigstore's TUF repository by the release build).
func EmbeddedTrustedRoot() []byte { return append([]byte{}, trustedRootJSON...) }

// ParseTrustedRoot reads a Sigstore trusted root that can verify a
// keyless signature: it must hold a certificate authority, a
// transparency log and a CT log.
func ParseTrustedRoot(raw []byte) (*root.TrustedRoot, error) {
	tr, err := root.NewTrustedRootFromJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("benchsig: trusted root: %w", err)
	}
	if len(tr.FulcioCertificateAuthorities()) == 0 || len(tr.RekorLogs()) == 0 ||
		len(tr.CTLogs()) == 0 {
		return nil, errors.New("benchsig: trusted root lacks a certificate authority, " +
			"a transparency log or a CT log")
	}
	return tr, nil
}

// NewReleaseVerifier verifies pg_sage release signatures against the
// embedded trusted root.
func NewReleaseVerifier() (*Verifier, error) {
	tr, err := ParseTrustedRoot(trustedRootJSON)
	if err != nil {
		return nil, err
	}
	return NewVerifier(tr, ReleaseIdentity())
}

// Verify checks bundleJSON (a Sigstore bundle, as cosign sign-blob
// --bundle writes it) as a signature of report.
func (v *Verifier) Verify(report, bundleJSON []byte) (Signature, error) {
	if v == nil {
		return Signature{}, errors.New("benchsig: no verifier")
	}
	b := &bundle.Bundle{}
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return Signature{}, fmt.Errorf("%w: %v", ErrMalformedBundle, err)
	}
	return v.VerifyEntity(report, b)
}

// VerifyEntity checks a parsed signed entity as a signature of report.
func (v *Verifier) VerifyEntity(report []byte, entity verify.SignedEntity) (Signature,
	error) {
	if v == nil {
		return Signature{}, errors.New("benchsig: no verifier")
	}
	if entity == nil {
		return Signature{}, fmt.Errorf("%w: no signed entity", ErrMalformedBundle)
	}
	if len(report) == 0 {
		return Signature{}, fmt.Errorf("%w: empty report", ErrInvalidSignature)
	}
	content, err := entity.SignatureContent()
	if err != nil {
		return Signature{}, fmt.Errorf("%w: %v", ErrMalformedBundle, err)
	}
	if content == nil || content.MessageSignatureContent() == nil {
		return Signature{}, fmt.Errorf("%w: not a signature of a file (cosign sign-blob)",
			ErrInvalidSignature)
	}
	res, err := v.verifier.Verify(entity, verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(report)), verify.WithCertificateIdentity(v.identity)))
	if err != nil {
		return Signature{}, fmt.Errorf("%w: %v", ErrInvalidSignature, err)
	}
	return signatureOf(res)
}

func signatureOf(res *verify.VerificationResult) (Signature, error) {
	if res == nil || res.Signature == nil || res.Signature.Certificate == nil {
		return Signature{}, fmt.Errorf("%w: no certificate in the result", ErrInvalidSignature)
	}
	cert := res.Signature.Certificate
	sig := Signature{Identity: cert.SubjectAlternativeName, Issuer: cert.Issuer,
		Commit: strings.ToLower(cert.SourceRepositoryDigest), Ref: cert.SourceRepositoryRef}
	for _, ts := range res.VerifiedTimestamps {
		if sig.SignedAt.IsZero() || ts.Timestamp.Before(sig.SignedAt) {
			sig.SignedAt = ts.Timestamp.UTC()
		}
	}
	return sig, nil
}
