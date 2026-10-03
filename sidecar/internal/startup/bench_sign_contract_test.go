package startup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Roadmap 1.1 release contract (coordinator decision 2026-10-03): the
// bench reports are signed keyless by the bench-sign job. A release
// (v* tag) and its versioned image never ship without verified signed
// reports: the dependency is hard. A master push still publishes :edge
// when bench-sign fails or is skipped, without bench reports (the
// sidecar starts cleanly with none shipped).

type ciStep struct {
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	With map[string]string `yaml:"with"`
}

type ciJob struct {
	Needs       any               `yaml:"needs"`
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []ciStep          `yaml:"steps"`
}

func ciJobs(t *testing.T) map[string]ciJob {
	t.Helper()
	path := filepath.Join(wave5SidecarRoot(t), "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf struct {
		Jobs map[string]ciJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf.Jobs
}

func (j ciJob) needs() []string {
	switch n := j.Needs.(type) {
	case string:
		return []string{n}
	case []any:
		var out []string
		for _, v := range n {
			out = append(out, v.(string))
		}
		return out
	}
	return nil
}

// expr is an if-expression with its whitespace collapsed.
func expr(s string) string { return strings.Join(strings.Fields(s), " ") }

// signedDownloads are the job's steps that download a bench-sign artifact.
func signedDownloads(j ciJob) []ciStep {
	var out []ciStep
	for _, s := range j.Steps {
		name := s.With["name"]
		if strings.HasPrefix(s.Uses, "actions/download-artifact") &&
			(name == "pgincidentbench-signed" || name == "sigstore-trusted-root") {
			out = append(out, s)
		}
	}
	return out
}

func TestBenchSignJobSignsKeyless(t *testing.T) {
	sign, ok := ciJobs(t)["bench-sign"]
	if !ok {
		t.Fatal("ci.yml has no bench-sign job")
	}
	if sign.Permissions["id-token"] != "write" || !slices.Contains(sign.needs(), "test") {
		t.Fatalf("bench-sign = needs %v, permissions %v; want test and id-token: write",
			sign.needs(), sign.Permissions)
	}
}

// A release never ships without verified signed reports.
func TestReleaseHardDependsOnBenchSign(t *testing.T) {
	release := ciJobs(t)["release"]
	if !slices.Contains(release.needs(), "bench-sign") {
		t.Fatalf("release needs %v, want bench-sign", release.needs())
	}
	// always() / failure() would let a release run after bench-sign failed.
	for _, escape := range []string{"always()", "failure()", "!cancelled()"} {
		if strings.Contains(release.If, escape) {
			t.Fatalf("release if %q contains %s", release.If, escape)
		}
	}
	for _, s := range signedDownloads(release) {
		if s.If != "" {
			t.Fatalf("release downloads %s conditionally (%q)", s.With["name"], s.If)
		}
	}
	if len(signedDownloads(release)) != 2 {
		t.Fatal("release does not download the signed reports and the trusted root")
	}
}

// The image job: hard on tags, soft on master.
func TestDockerSoftOnMasterHardOnTags(t *testing.T) {
	docker := ciJobs(t)["docker"]
	if !slices.Contains(docker.needs(), "bench-sign") {
		t.Fatalf("docker needs %v, want bench-sign (it ships the signed reports)",
			docker.needs())
	}
	cond := expr(docker.If)
	for _, clause := range []string{
		// Runs although bench-sign failed or was skipped...
		"always()",
		// ...but never after a failed test or lint job...
		"needs.test.result == 'success'", "needs.lint.result == 'success'",
		// ...and a tag only with signed reports.
		"(github.ref == 'refs/heads/master' || (startsWith(github.ref, 'refs/tags/v') && " +
			"needs.bench-sign.result == 'success'))",
	} {
		if !strings.Contains(cond, clause) {
			t.Errorf("docker if %q lacks %q", cond, clause)
		}
	}
	downloads := signedDownloads(docker)
	if len(downloads) != 2 {
		t.Fatalf("docker downloads %d signed artifacts, want 2", len(downloads))
	}
	// Without bench-sign the image is built without reports instead of
	// failing on a missing artifact.
	for _, s := range downloads {
		if expr(s.If) != "needs.bench-sign.result == 'success'" {
			t.Errorf("docker download of %s has if %q, want it gated on bench-sign",
				s.With["name"], s.If)
		}
	}
}
