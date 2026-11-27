package startup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ci-06: the image HEALTHCHECK runs the sidecar's own healthcheck
// subcommand, which follows api.listen_addr and SAGE_TLS_CERT/SAGE_TLS_KEY.
// The former contract pinned "curl http://localhost:8080/health", which
// marks every TLS deployment and every non-default port unhealthy.

// dockerInstruction returns the named instruction with its line
// continuations joined, or "" when the Dockerfile has none.
func dockerInstruction(dockerfile, name string) string {
	lines := strings.Split(strings.ReplaceAll(dockerfile, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), name+" ") {
			continue
		}
		joined := strings.TrimSpace(lines[i])
		for strings.HasSuffix(joined, "\\") && i+1 < len(lines) {
			i++
			joined = strings.TrimSuffix(joined, "\\") + " " + strings.TrimSpace(lines[i])
		}
		return joined
	}
	return ""
}

func readSidecarFile(t *testing.T, rel ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{wave5SidecarRoot(t)}, rel...)...)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func TestContainerHealthcheckUsesSubcommand(t *testing.T) {
	dockerfile := readSidecarFile(t, "Dockerfile")
	check := dockerInstruction(dockerfile, "HEALTHCHECK")
	if check == "" {
		t.Fatal("Dockerfile has no HEALTHCHECK")
	}
	compact := strings.Join(strings.Fields(check), "")
	if !strings.Contains(compact, `CMD["sage-sidecar","healthcheck"]`) {
		t.Errorf("HEALTHCHECK %q does not run [\"sage-sidecar\",\"healthcheck\"]", check)
	}
	for _, banned := range []string{"/api/v1/metrics", "curl", "localhost:8080", "http://"} {
		if strings.Contains(check, banned) {
			t.Errorf("HEALTHCHECK %q still contains %q", check, banned)
		}
	}
	// CHECK-02: curl was in the runtime image only for the old probe.
	if strings.Contains(dockerfile, "curl") {
		t.Error("Dockerfile still installs or calls curl")
	}
}

func TestContainerHealthcheckSmokeJobCoversTLSAndPlain(t *testing.T) {
	path := filepath.Join(wave5SidecarRoot(t), "..", ".github", "workflows", "ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf struct {
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
			Needs any `yaml:"needs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	job, ok := wf.Jobs["container-health"]
	if !ok {
		t.Fatal("ci.yml has no container-health job")
	}
	if strings.Contains(job.If, "push") {
		t.Errorf("container-health if = %q: it must run on pull requests too", job.If)
	}
	runs := false
	for _, step := range job.Steps {
		runs = runs || strings.Contains(step.Run, "scripts/ci/container-healthcheck.sh")
	}
	if !runs {
		t.Fatal("container-health does not run scripts/ci/container-healthcheck.sh")
	}
	docker := ciJob{Needs: wf.Jobs["docker"].Needs, If: wf.Jobs["docker"].If}
	if !slices.Contains(docker.needs(), "container-health") {
		t.Errorf("docker needs %v: an image must not publish past a failing healthcheck",
			docker.needs())
	}
	if cond := expr(docker.If); strings.Contains(cond, "always()") &&
		!strings.Contains(cond, "needs.container-health.result == 'success'") {
		t.Errorf("docker if %q publishes past a failed container-health job", cond)
	}
	script := readSidecarFile(t, "..", "scripts", "ci", "container-healthcheck.sh")
	for _, want := range []string{"SAGE_TLS_CERT", "SAGE_TLS_KEY", "openssl req",
		"{{.State.Health.Status}}", "healthy", "SAGE_DATABASE_URL"} {
		if !strings.Contains(script, want) {
			t.Errorf("container-healthcheck.sh does not contain %q", want)
		}
	}
}

func TestDockerInstructionJoinsContinuations(t *testing.T) {
	got := dockerInstruction("FROM x\r\nHEALTHCHECK --interval=1s \\\r\n"+
		"    CMD [\"a\"]\r\nUSER y\r\n", "HEALTHCHECK")
	if got != `HEALTHCHECK --interval=1s  CMD ["a"]` {
		t.Fatalf("joined instruction = %q", got)
	}
	if dockerInstruction("FROM x\n", "HEALTHCHECK") != "" {
		t.Fatal("missing instruction: want empty")
	}
}
