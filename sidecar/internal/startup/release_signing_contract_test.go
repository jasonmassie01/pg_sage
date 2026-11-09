package startup

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ci-05 (next): release checksums are signed keyless with the identity and
// issuer the bench reports already use, every archive has an SBOM, and the
// image carries provenance, an SBOM and a cosign signature. Whether the
// signatures verify on real release assets is CHECK-22 (MANUAL).

var exactModuleVersion = regexp.MustCompile(`@v\d+\.\d+\.\d+(\s|$)`)

// benchVerifyEnv is the identity and issuer bench-sign verifies with.
func benchVerifyEnv(t *testing.T, wf relWorkflow) (string, string) {
	t.Helper()
	job := relJobNamed(t, wf, "bench-sign")
	i := stepIndex(job, "cosign verify-blob")
	if i < 0 {
		t.Fatal("bench-sign has no cosign verify-blob step")
	}
	env := job.Steps[i].Env
	if env["IDENTITY"] == "" || env["ISSUER"] == "" {
		t.Fatalf("bench-sign verify env = %v, want IDENTITY and ISSUER", env)
	}
	return env["IDENTITY"], env["ISSUER"]
}

// cosignInstaller is the uses: of bench-sign's cosign-installer step.
func cosignInstaller(t *testing.T, wf relWorkflow) string {
	t.Helper()
	job := relJobNamed(t, wf, "bench-sign")
	i := stepIndex(job, "sigstore/cosign-installer@")
	if i < 0 {
		t.Fatal("bench-sign installs no cosign")
	}
	return job.Steps[i].Uses
}

// assertSignsLikeTheBench checks that job installs cosign with bench-sign's
// pinned installer before step at, and verifies after it with bench-sign's
// identity and issuer. It returns the verify step's index.
func assertSignsLikeTheBench(t *testing.T, wf relWorkflow, name string, at int, verify string) int {
	t.Helper()
	job := relJobNamed(t, wf, name)
	install := stepIndex(job, cosignInstaller(t, wf))
	if install < 0 || install > at {
		t.Errorf("%s installs cosign at step %d, want %q before step %d",
			name, install, cosignInstaller(t, wf), at)
	}
	v := stepIndex(job, verify, "--certificate-identity \"$IDENTITY\"",
		"--certificate-oidc-issuer \"$ISSUER\"")
	if v < 0 || v < at {
		t.Fatalf("%s verifies at step %d, want a %q step after step %d", name, v, verify, at)
	}
	identity, issuer := benchVerifyEnv(t, wf)
	if env := job.Steps[v].Env; env["IDENTITY"] != identity || env["ISSUER"] != issuer {
		t.Errorf("%s verifies as %v, want bench-sign's IDENTITY %q and ISSUER %q",
			name, env, identity, issuer)
	}
	if p := job.Permissions; p["id-token"] != "write" {
		t.Errorf("%s permissions %v, want id-token: write to sign keyless", name, p)
	}
	return v
}

func TestGoreleaserSignsChecksums(t *testing.T) {
	cfg := loadGoreleaser(t)
	if len(cfg.Signs) != 1 {
		t.Fatalf(".goreleaser.yml has %d signs entries, want one over the checksum", len(cfg.Signs))
	}
	sign := cfg.Signs[0]
	if sign.Cmd != "cosign" || sign.Artifacts != "checksum" ||
		sign.Signature != "${artifact}.sigstore.json" {
		t.Errorf("signs = %+v, want cosign over the checksum into ${artifact}.sigstore.json", sign)
	}
	for _, arg := range []string{"sign-blob", "--bundle=${signature}", "${artifact}", "--yes"} {
		if !slices.Contains(sign.Args, arg) {
			t.Errorf("signs args %q lack %q", sign.Args, arg)
		}
	}
	if len(cfg.Sboms) != 1 || cfg.Sboms[0].Artifacts != "archive" ||
		!slices.Equal(cfg.Sboms[0].Documents, []string{"{{ .ArtifactName }}.sbom.json"}) {
		t.Errorf("sboms = %+v, want one SBOM per archive named <archive>.sbom.json", cfg.Sboms)
	}
	wf := loadRelWorkflow(t)
	release := relJobNamed(t, wf, "release")
	build := stepIndex(release, "goreleaser release")
	if build < 0 {
		t.Fatal("release runs no goreleaser release")
	}
	run := release.Steps[build].Run
	// cosign in the container needs the runner's OIDC request token.
	for _, want := range []string{"-e ACTIONS_ID_TOKEN_REQUEST_URL",
		"-e ACTIONS_ID_TOKEN_REQUEST_TOKEN", "/opt/release/bin:$PATH"} {
		if !strings.Contains(run, want) {
			t.Errorf("goreleaser step lacks %q:\n%s", want, run)
		}
	}
	syft := stepIndex(release, "github.com/anchore/syft/cmd/syft@")
	if syft < 0 || syft > build || !exactModuleVersion.MatchString(release.Steps[syft].Run) {
		t.Errorf("release installs syft at step %d, want an exact version before goreleaser %d",
			syft, build)
	}
	verify := assertSignsLikeTheBench(t, wf, "release", build,
		"cosign verify-blob --bundle dist/checksums.txt.sigstore.json")
	if !strings.Contains(release.Steps[verify].Run, "sha256sum") {
		t.Error("the release verify step does not check the archives against checksums.txt")
	}
	if publish := stepIndex(release, "gh release edit", "--draft=false"); publish < verify {
		t.Errorf("release publishes at step %d, before the signature check %d", publish, verify)
	}
}

func TestInstallDocsShowHowToVerify(t *testing.T) {
	docs := readRepoFile(t, filepath.Join(wave5SidecarRoot(t), "..", "docs", "installation.md"))
	for _, want := range []string{
		"cosign verify-blob", "--bundle checksums.txt.sigstore.json",
		"sha256sum --ignore-missing -c checksums.txt",
		"--certificate-identity " +
			"\"https://github.com/jasonmassie01/pg_sage/.github/workflows/ci.yml@refs/tags/v",
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com",
		".sbom.json",
	} {
		if !strings.Contains(docs, want) {
			t.Errorf("docs/installation.md lacks %q", want)
		}
	}
}
