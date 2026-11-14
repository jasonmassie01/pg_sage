package startup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ci-05 (then): the image is built for linux/amd64 and linux/arm64 on
// native runners (no QEMU), each pushed by digest and smoke-tested, then
// merged into one tagged, signed manifest list. A pull request that
// touches the release files runs a goreleaser dry run, and release notes
// come from the tag's CHANGELOG section.

func TestDockerBuildIsMultiArch(t *testing.T) {
	wf := loadRelWorkflow(t)
	docker := relJobNamed(t, wf, "docker")
	want := []map[string]string{
		{"platform": "linux/amd64", "arch": "amd64", "runner": "ubuntu-latest"},
		{"platform": "linux/arm64", "arch": "arm64", "runner": "ubuntu-24.04-arm"},
	}
	got := docker.Strategy.Matrix.Include
	if len(got) != len(want) {
		t.Fatalf("docker matrix include = %v, want %v", got, want)
	}
	for i := range want {
		if fmt.Sprint(got[i]) != fmt.Sprint(want[i]) {
			t.Errorf("docker matrix leg %d = %v, want %v", i, got[i], want[i])
		}
	}
	if docker.RunsOn != "${{ matrix.runner }}" {
		t.Errorf("docker runs-on %q, want the leg's native runner", docker.RunsOn)
	}
	if ff := docker.Strategy.FailFast; ff != nil && !*ff {
		t.Error("docker fail-fast is off: one arch could be tagged without the other")
	}
	if strings.Contains(ciWorkflowText(t), "docker/setup-qemu-action") {
		t.Error("ci.yml emulates with QEMU; each arch builds on its own runner")
	}
	b := slices.IndexFunc(docker.Steps, func(s relStep) bool {
		return strings.HasPrefix(s.Uses, "docker/build-push-action")
	})
	if b < 0 {
		t.Fatal("docker has no build-push-action step")
	}
	with := docker.Steps[b].With
	if with["platforms"] != "${{ matrix.platform }}" {
		t.Errorf("docker builds platforms %q, want the leg's platform", with["platforms"])
	}
	for _, o := range []string{"push-by-digest=true", "name-canonical=true", "push=true"} {
		if !strings.Contains(with["outputs"], o) {
			t.Errorf("docker build outputs %q lack %s", with["outputs"], o)
		}
	}
	if with["tags"] != "" || with["push"] == "true" {
		t.Errorf("docker build tags %q push %q: a leg pushes by digest only, the merge tags",
			with["tags"], with["push"])
	}
	if stepIndex(docker, "actions/upload-artifact") < 0 {
		t.Error("docker uploads no digest for the merge job")
	}
}

func TestImageIsSignedWithProvenance(t *testing.T) {
	wf := loadRelWorkflow(t)
	docker := relJobNamed(t, wf, "docker")
	b := stepIndex(docker, "docker/build-push-action")
	if b < 0 {
		t.Fatal("docker has no build-push-action step")
	}
	if w := docker.Steps[b].With; w["provenance"] != "mode=max" || w["sbom"] != "true" {
		t.Errorf("docker build provenance %q sbom %q, want mode=max and true",
			w["provenance"], w["sbom"])
	}
	merge := relJobNamed(t, wf, "docker-manifest")
	if !slices.Equal(merge.needs(), []string{"docker"}) {
		t.Errorf("docker-manifest needs %v, want [docker]", merge.needs())
	}
	create := stepIndex(merge, "docker buildx imagetools create", "DOCKER_METADATA_OUTPUT_JSON")
	sign := stepIndex(merge, "cosign sign --yes", "ghcr.io/jasonmassie01/pg_sage@")
	if create < 0 || sign < create {
		t.Fatalf("docker-manifest creates the list at step %d and signs at %d; "+
			"want it signed after it is created", create, sign)
	}
	assertSignsLikeTheBench(t, wf, "docker-manifest", sign, "cosign verify ")
	if p := merge.Permissions; p["packages"] != "write" || p["contents"] != "read" {
		t.Errorf("docker-manifest permissions %v, want packages: write, contents: read", p)
	}
}

// The merge runs exactly when every leg pushed and smoke-tested, and the
// live arm never holds it back.
func TestDockerManifestFollowsTheLegs(t *testing.T) {
	wf := loadRelWorkflow(t)
	merge := relJobNamed(t, wf, "docker-manifest")
	for _, ref := range []string{"refs/tags/v2.6.0", "refs/heads/master"} {
		// A grandparent the docker job tolerates on master (bench-sign
		// skipped) must not skip the merge.
		st := pushState(ref, merge.needs())
		st.ctx["needs.bench-sign.result"] = "skipped"
		assertRuns(t, "docker-manifest on "+ref, merge.If, st, true)
		for _, r := range []string{"failure", "skipped"} {
			assertRuns(t, "docker-manifest on "+ref+" (docker "+r+")", merge.If,
				st.with("docker", r), false)
		}
	}
	jobs := liveCIJobs(t)
	seen := map[string]bool{}
	var waits func(string) bool
	waits = func(name string) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		return slices.ContainsFunc(jobs[name].needs(), func(n string) bool {
			return n == "bench-live" || waits(n)
		})
	}
	if waits("docker-manifest") {
		t.Fatal("docker-manifest waits for bench-live")
	}
}

func TestInstallDocsShowImageVerification(t *testing.T) {
	docs := readRepoFile(t, filepath.Join(wave5SidecarRoot(t), "..", "docs", "installation.md"))
	for _, want := range []string{"cosign verify ghcr.io/jasonmassie01/pg_sage:", "linux/arm64",
		"docker buildx imagetools inspect"} {
		if !strings.Contains(docs, want) {
			t.Errorf("docs/installation.md lacks %q", want)
		}
	}
}

func TestReleaseNotesComeFromTheChangelog(t *testing.T) {
	release := relJobNamed(t, loadRelWorkflow(t), "release")
	notes := stepIndex(release, "scripts/ci/release-notes.sh \"$GITHUB_REF_NAME\" CHANGELOG.md")
	build := stepIndex(release, "goreleaser release")
	if notes < 0 || build < 0 || notes > build {
		t.Fatalf("release writes notes at step %d, goreleaser at %d; want notes first",
			notes, build)
	}
	if !strings.Contains(release.Steps[build].Run, "--release-notes /opt/release/notes.md") {
		t.Errorf("goreleaser does not take the CHANGELOG notes:\n%s", release.Steps[build].Run)
	}
}

func TestReleaseDryRunOnPullRequests(t *testing.T) {
	dry := relJobNamed(t, loadRelWorkflow(t), "release-dry-run")
	pr := ghState{ctx: map[string]string{"github.event_name": "pull_request"}}
	assertRuns(t, "release-dry-run on a pull request", dry.If, pr, true)
	push := pushState("refs/heads/master", nil)
	assertRuns(t, "release-dry-run on a push", dry.If, push, false)
	if w := grantedWrites(dry); len(w) != 0 {
		t.Errorf("release-dry-run is granted %v; it runs pull request code", w)
	}
	changes := slices.IndexFunc(dry.Steps, func(s relStep) bool { return s.ID == "changes" })
	if changes < 0 {
		t.Fatal("release-dry-run has no changes step")
	}
	gated := 0
	for _, s := range dry.Steps[changes+1:] {
		if expr(s.If) == "steps.changes.outputs.changed == 'true'" {
			gated++
		}
	}
	if gated != len(dry.Steps)-changes-1 {
		t.Errorf("%d of %d steps after the changes step are gated on it",
			gated, len(dry.Steps)-changes-1)
	}
	for _, want := range [][]string{{"goreleaser check"},
		{"goreleaser release --snapshot", "--skip=before,validate,publish,sign"},
		{"scripts/ci/smoke-version.sh", "dist/metadata.json", "dist/pg_sage_linux_amd64.tar.gz"}} {
		if stepIndex(dry, want...) < 0 {
			t.Errorf("release-dry-run has no step running %q", want)
		}
	}
}

// The changes step, run in a scratch repository: only the release files
// trigger the dry run, and a long diff cannot break its pipe.
func TestReleaseDryRunDetectsReleaseFiles(t *testing.T) {
	dry := relJobNamed(t, loadRelWorkflow(t), "release-dry-run")
	i := slices.IndexFunc(dry.Steps, func(s relStep) bool { return s.ID == "changes" })
	if i < 0 {
		t.Fatal("release-dry-run has no changes step")
	}
	script := dry.Steps[i].Run
	if dry.Steps[i].Env["BASE"] != "${{ github.event.pull_request.base.sha }}" {
		t.Fatalf("changes step env %v, want BASE from the pull request", dry.Steps[i].Env)
	}
	cases := []struct {
		files []string
		want  string
	}{
		{[]string{".goreleaser.yml"}, "true"},
		{[]string{"sidecar/Dockerfile"}, "true"},
		{[]string{".github/workflows/ci.yml"}, "true"},
		{[]string{"scripts/ci/smoke-version.sh"}, "true"},
		{[]string{"scripts/ci/release-notes.sh"}, "true"},
		{[]string{"scripts/ci/line-limits.sh"}, "true"},        // ci-30
		{[]string{"scripts/ci/ledger-append-only.sh"}, "true"}, // ci-30
		{[]string{"scripts/ci/line-limits.allow"}, "false"},    // ci-30: split PRs
		{[]string{"scripts/ci/line-limits_test.sh"}, "false"},  // ci-30
		{[]string{"README.md", "sidecar/main.go"}, "false"},
		{[]string{"sidecar/Dockerfile.dev", "x/.goreleaser.yml"}, "false"},
		{append(manyFiles(3000), "sidecar/Dockerfile"), "true"},
	}
	for _, c := range cases {
		if got := runChangesStep(t, script, c.files); got != c.want {
			t.Errorf("changed files %d (last %q): changed=%q, want %q",
				len(c.files), c.files[len(c.files)-1], got, c.want)
		}
	}
}

func manyFiles(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("docs/generated/page-%04d.md", i)
	}
	return out
}

// runChangesStep commits a base, then files on top, runs the step script
// with BASE at the base commit and returns the changed output.
func runChangesStep(t *testing.T, script string, files []string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t",
			"-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	for _, f := range files {
		path := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "change")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("the changes step is bash and bash is missing: %v", err)
	}
	out := filepath.Join(t.TempDir(), "output")
	cmd := exec.Command(bash, "-eo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BASE="+base, "GITHUB_OUTPUT="+out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("changes step: %v: %s", err, msg)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "changed="); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("changes step wrote no changed= line: %q", raw)
	return ""
}
