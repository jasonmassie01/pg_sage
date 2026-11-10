package startup

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ci-05: a v* tag ships only past the whole green matrix, with a
// read-only token by default, and only binaries and images that answer
// --version with the tag's version and the libpg_query parser.
//
// No PostgreSQL or concurrent-access surface: these tests read committed
// workflow and goreleaser files and run the release scripts on fixtures.
// Whether a real tag run prints the version line is CHECK-21 (MANUAL).

type relStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

type relJob struct {
	RunsOn      string            `yaml:"runs-on"`
	Needs       any               `yaml:"needs"`
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Strategy    struct {
		FailFast *bool `yaml:"fail-fast"`
		Matrix   struct {
			Include []map[string]string `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Outputs map[string]string `yaml:"outputs"`
	Steps   []relStep         `yaml:"steps"`
}

func (j relJob) needs() []string { return ciJob{Needs: j.Needs}.needs() }

type relWorkflow struct {
	Permissions map[string]string `yaml:"permissions"`
	Jobs        map[string]relJob `yaml:"jobs"`
}

func loadRelWorkflow(t *testing.T) relWorkflow {
	t.Helper()
	var wf relWorkflow
	if err := yaml.Unmarshal([]byte(ciWorkflowText(t)), &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	return wf
}

func relJobNamed(t *testing.T, wf relWorkflow, name string) relJob {
	t.Helper()
	job, ok := wf.Jobs[name]
	if !ok {
		t.Fatalf("ci.yml has no %s job", name)
	}
	return job
}

// stepIndex is the index of the first step whose run or uses contains every
// needle, or -1.
func stepIndex(job relJob, needles ...string) int {
	for i, s := range job.Steps {
		text := s.Uses + "\n" + s.Run
		missing := func(n string) bool { return !strings.Contains(text, n) }
		if !slices.ContainsFunc(needles, missing) {
			return i
		}
	}
	return -1
}

// releaseGates are the jobs that verify a commit. A v* tag publishes
// nothing that any of them failed on.
var releaseGates = []string{"test", "test-arm64", "integration-matrix", "agent-servers",
	"lint", "security", "container-health"}

func TestReleaseNeedsTheWholeMatrix(t *testing.T) {
	wf := loadRelWorkflow(t)
	for _, gate := range releaseGates {
		relJobNamed(t, wf, gate)
	}
	release := relJobNamed(t, wf, "release")
	want := append(slices.Clone(releaseGates), "bench-sign", "safety-bench-sign")
	for _, n := range want {
		if !slices.Contains(release.needs(), n) {
			t.Errorf("release needs %v, missing %s", release.needs(), n)
		}
	}
	tag := pushState("refs/tags/v2.6.0", release.needs())
	assertRuns(t, "release", release.If, tag, true)
	for _, n := range want {
		for _, result := range []string{"failure", "skipped", "cancelled"} {
			assertRuns(t, "release ("+n+" "+result+")", release.If, tag.with(n, result), false)
		}
	}
	assertRuns(t, "release on master", release.If,
		pushState("refs/heads/master", release.needs()), false)
}

func TestDockerOnATagNeedsTheWholeMatrix(t *testing.T) {
	docker := relJobNamed(t, loadRelWorkflow(t), "docker")
	want := append(slices.Clone(releaseGates), "bench-sign")
	for _, n := range want {
		if !slices.Contains(docker.needs(), n) {
			t.Errorf("docker needs %v, missing %s", docker.needs(), n)
		}
	}
	tag := pushState("refs/tags/v2.6.0", docker.needs())
	assertRuns(t, "docker on a tag", docker.If, tag, true)
	for _, n := range want {
		assertRuns(t, "docker on a tag ("+n+" failed)", docker.If, tag.with(n, "failure"), false)
	}
}

// :edge publishing on master is unchanged: it waits for test, lint,
// security and the container health check, not for the slower legs.
func TestDockerEdgeOnMasterKeepsItsRule(t *testing.T) {
	docker := relJobNamed(t, loadRelWorkflow(t), "docker")
	master := pushState("refs/heads/master", docker.needs())
	assertRuns(t, "docker on master", docker.If, master, true)
	for _, n := range []string{"integration-matrix", "bench-sign", "agent-servers", "test-arm64"} {
		assertRuns(t, "docker on master ("+n+" failed)", docker.If, master.with(n, "failure"), true)
	}
	for _, n := range []string{"test", "lint", "security", "container-health"} {
		assertRuns(t, "docker on master ("+n+" failed)", docker.If,
			master.with(n, "failure"), false)
	}
	pr := master
	pr.ctx = map[string]string{"github.event_name": "pull_request",
		"github.ref": "refs/pull/1/merge"}
	assertRuns(t, "docker on a pull request", docker.If, pr, false)
}

func assertRuns(t *testing.T, what, cond string, st ghState, want bool) {
	t.Helper()
	got, err := ghIf(cond, st)
	if err != nil {
		t.Fatalf("%s: evaluate if %q: %v", what, cond, err)
	}
	if got != want {
		t.Errorf("%s: runs = %v, want %v (if %q)", what, got, want, expr(cond))
	}
}

// writeMarkers are the commands that need a write scope of the token.
var writeMarkers = []struct{ scope, needle string }{
	{"contents", "gh release "},
	{"packages", "imagetools create"},
	{"packages", "cosign sign "},
	{"id-token", "cosign sign"},
}

// neededWrites is the set of write scopes a job's steps use. A real
// goreleaser release writes contents, and id-token when it signs.
func neededWrites(job relJob, goreleaserSigns bool) []string {
	set := map[string]bool{}
	for _, s := range job.Steps {
		for _, m := range writeMarkers {
			if strings.Contains(s.Run, m.needle) {
				set[m.scope] = true
			}
		}
		if strings.Contains(s.Run, "goreleaser release") && !strings.Contains(s.Run, "--snapshot") {
			set["contents"] = true
			if goreleaserSigns {
				set["id-token"] = true
			}
		}
		if strings.HasPrefix(s.Uses, "docker/build-push-action") &&
			(s.With["push"] == "true" || strings.Contains(s.With["outputs"], "push=true")) {
			set["packages"] = true
		}
	}
	return sortedKeys(set)
}

func grantedWrites(job relJob) []string {
	set := map[string]bool{}
	for scope, level := range job.Permissions {
		if level == "write" {
			set[scope] = true
		}
	}
	return sortedKeys(set)
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// permissionProblems says where a job's write grants and its writes differ.
func permissionProblems(name string, job relJob, goreleaserSigns bool) []string {
	need, have := neededWrites(job, goreleaserSigns), grantedWrites(job)
	var out []string
	for _, s := range need {
		if !slices.Contains(have, s) {
			out = append(out, name+" writes "+s+" without its own permissions block granting it")
		}
	}
	for _, s := range have {
		if !slices.Contains(need, s) {
			out = append(out, name+" is granted "+s+": write but nothing in it writes "+s)
		}
	}
	return out
}

func TestWorkflowDefaultsToReadOnlyToken(t *testing.T) {
	wf := loadRelWorkflow(t)
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Fatalf("top-level permissions = %v, want exactly contents: read", wf.Permissions)
	}
	signs := len(loadGoreleaser(t).Signs) > 0
	writers := 0
	for name, job := range wf.Jobs {
		for _, p := range permissionProblems(name, job, signs) {
			t.Error(p)
		}
		if len(grantedWrites(job)) > 0 {
			writers++
		}
	}
	// docker and release at least: an empty count means the markers broke.
	if writers < 2 {
		t.Fatalf("found %d jobs with write grants, want docker and release among them", writers)
	}
}

func TestPermissionProblemsFlagsMismatchedGrants(t *testing.T) {
	cases := []struct {
		name string
		job  relJob
		want []string
	}{
		{"release with no block", relJob{Steps: []relStep{{Run: "gh release edit v1"}}},
			[]string{"writes contents"}},
		{"unused packages write", relJob{Permissions: map[string]string{"packages": "write"},
			Steps: []relStep{{Run: "go test ./..."}}}, []string{"granted packages"}},
		{"signing without id-token", relJob{Permissions: map[string]string{"contents": "read"},
			Steps: []relStep{{Run: "cosign sign-blob --yes f"}}}, []string{"writes id-token"}},
		{"goreleaser signing", relJob{Permissions: map[string]string{"contents": "write"},
			Steps: []relStep{{Run: "goreleaser release --clean"}}}, []string{"writes id-token"}},
		{"push by digest", relJob{Steps: []relStep{{Uses: "docker/build-push-action@v6",
			With: map[string]string{"outputs": "type=image,push-by-digest=true,push=true"}}}},
			[]string{"writes packages"}},
		{"snapshot writes nothing",
			relJob{Steps: []relStep{{Run: "goreleaser release --snapshot"}}}, nil},
		{"zero job", relJob{}, nil},
	}
	for _, c := range cases {
		got := permissionProblems("j", c.job, true)
		if len(got) != len(c.want) {
			t.Errorf("%s: problems %q, want %d matching %q", c.name, got, len(c.want), c.want)
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: problem %q, want it to say %q", c.name, got[i], w)
			}
		}
	}
}

type goreleaserCfg struct {
	Signs []struct {
		Cmd       string   `yaml:"cmd"`
		Signature string   `yaml:"signature"`
		Args      []string `yaml:"args"`
		Artifacts string   `yaml:"artifacts"`
	} `yaml:"signs"`
	Sboms []struct {
		Artifacts string   `yaml:"artifacts"`
		Documents []string `yaml:"documents"`
	} `yaml:"sboms"`
	Release struct {
		Draft                bool   `yaml:"draft"`
		ReplaceExistingDraft bool   `yaml:"replace_existing_draft"`
		UseExistingDraft     bool   `yaml:"use_existing_draft"`
		NameTemplate         string `yaml:"name_template"`
	} `yaml:"release"`
}

func loadGoreleaser(t *testing.T) goreleaserCfg {
	t.Helper()
	path := filepath.Join(wave5SidecarRoot(t), "..", ".goreleaser.yml")
	var cfg goreleaserCfg
	if err := yaml.Unmarshal([]byte(readRepoFile(t, path)), &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}

// goreleaser publishes a draft; the smoke step runs the archived binary;
// only then is the release made public.
func TestReleaseSmokeChecksArchivedBinary(t *testing.T) {
	if !loadGoreleaser(t).Release.Draft {
		t.Fatal(".goreleaser.yml release.draft is false: " +
			"the release is public before its smoke test")
	}
	release := relJobNamed(t, loadRelWorkflow(t), "release")
	build := stepIndex(release, "goreleaser release")
	smoke := stepIndex(release, "dist/pg_sage_linux_amd64.tar.gz",
		"scripts/ci/smoke-version.sh \"${GITHUB_REF_NAME#v}\"")
	publish := stepIndex(release, "gh release edit", "--draft=false")
	if build < 0 || smoke < 0 || publish < 0 {
		t.Fatalf("release steps: goreleaser %d, smoke %d, publish %d; want all three",
			build, smoke, publish)
	}
	if !(build < smoke && smoke < publish) {
		t.Fatalf("release step order goreleaser %d, smoke %d, publish %d; "+
			"want goreleaser, then smoke, then publish", build, smoke, publish)
	}
	for _, i := range []int{smoke, publish} {
		if s := release.Steps[i]; s.If != "" {
			t.Errorf("release step %q runs conditionally (%q)", s.Name, s.If)
		}
	}
	script := readRepoFile(t, filepath.Join(wave5SidecarRoot(t), "..", "scripts", "ci",
		"smoke-version.sh"))
	if !strings.Contains(script, "sql-ast: libpg_query") {
		t.Fatal("scripts/ci/smoke-version.sh does not check for sql-ast: libpg_query")
	}
}

// The image is run once by digest, with the version the build stamped.
func TestDockerSmokeChecksTheBuiltImage(t *testing.T) {
	docker := relJobNamed(t, loadRelWorkflow(t), "docker")
	build := slices.IndexFunc(docker.Steps, func(s relStep) bool {
		return strings.HasPrefix(s.Uses, "docker/build-push-action")
	})
	if build < 0 || docker.Steps[build].ID == "" {
		t.Fatal("docker has no build-push-action step with an id (its digest is smoke-tested)")
	}
	digest := "${{ steps." + docker.Steps[build].ID + ".outputs.digest }}"
	smoke := stepIndex(docker, "scripts/ci/smoke-version.sh", "docker run --rm", digest,
		"${{ steps.meta.outputs.version }}")
	if smoke < 0 {
		t.Fatalf("docker has no step smoke-testing %s with smoke-version.sh", digest)
	}
	if smoke < build {
		t.Fatalf("docker smoke step %d runs before the build %d", smoke, build)
	}
}
