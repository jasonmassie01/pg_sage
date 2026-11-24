package startup

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ci-03b: CI's PostgreSQL servers come from images with HypoPG,
// pg_hint_plan and pgaudit baked in, built on a base pinned by digest and
// published once per revision, so no job installs packages from
// apt.postgresql.org or restarts its server before the suites run.
//
// No concurrent-access surface: these tests read committed files. The
// PostgreSQL side is scripts/ci/verify-test-postgres.sh, which the image
// build runs on every image before it is pushed and every job runs on its
// server (the item report runs it on a local PG17 server).

const (
	ciImageRepo  = "ghcr.io/jasonmassie01/pg_sage-ci-pgvector"
	ciImageBuild = "mirror-ci-images.yml"
	ciVerify     = "scripts/ci/verify-test-postgres.sh"
)

// ciMajors are the PostgreSQL majors CI tests on.
var ciMajors = []string{"14", "15", "16", "17", "18"}

var (
	baseDigest  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	revisionTag = regexp.MustCompile(`^r[0-9]+$`)
	// ciImageRef matches a pull of the CI image; group 1 is the major
	// (literal or templated) and group 2 the revision suffix, if any.
	ciImageRef = regexp.MustCompile(`pg_sage-ci-pgvector:0\.8\.2-pg` +
		`([0-9]{2}|\$\{\{ matrix\.pg \}\}|\$major|\$\{major\})(-r[0-9]+)?`)
)

type imgBuildJob struct {
	Env      map[string]string `yaml:"env"`
	Strategy struct {
		FailFast *bool `yaml:"fail-fast"`
		Matrix   struct {
			Include []map[string]string `yaml:"include"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []relStep `yaml:"steps"`
}

type imgBuildWorkflow struct {
	On   map[string]any         `yaml:"on"`
	Jobs map[string]imgBuildJob `yaml:"jobs"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(wave5SidecarRoot(t), "..")
}

func loadImageBuild(t *testing.T) imgBuildJob {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".github", "workflows", ciImageBuild)
	var wf imgBuildWorkflow
	if err := yaml.Unmarshal([]byte(readRepoFile(t, path)), &wf); err != nil {
		t.Fatalf("parse %s: %v", ciImageBuild, err)
	}
	if _, ok := wf.On["schedule"]; ok {
		t.Errorf("%s rebuilds on a schedule: a rebuild would change what CI runs "+
			"under a published revision", ciImageBuild)
	}
	for _, trigger := range []string{"workflow_dispatch", "push"} {
		if _, ok := wf.On[trigger]; !ok {
			t.Errorf("%s has no %s trigger", ciImageBuild, trigger)
		}
	}
	job, ok := wf.Jobs["build"]
	if !ok {
		t.Fatalf("%s has no build job", ciImageBuild)
	}
	return job
}

// ciImageRevision is the revision the image build publishes, e.g. r1.
func ciImageRevision(t *testing.T, job imgBuildJob) string {
	t.Helper()
	rev := job.Env["REVISION"]
	if !revisionTag.MatchString(rev) {
		t.Fatalf("%s build REVISION = %q, want r<n>", ciImageBuild, rev)
	}
	return rev
}

func readImageFile(t *testing.T, name string) string {
	t.Helper()
	body := readRepoFile(t, filepath.Join(repoRoot(t), "scripts", "ci", "images", name))
	return strings.ReplaceAll(body, "\r\n", "\n")
}

// Image digests: every major is built on its own pinned base, so a
// rebuilt upstream tag never changes CI's server.
func TestCIImageBuildPinsEveryMajorByDigest(t *testing.T) {
	job := loadImageBuild(t)
	ciImageRevision(t, job)
	var majors []string
	seen := map[string]string{}
	for _, leg := range job.Strategy.Matrix.Include {
		majors = append(majors, leg["pg"])
		if !baseDigest.MatchString(leg["base"]) {
			t.Errorf("pg%s base = %q, want sha256:<64 hex>", leg["pg"], leg["base"])
		}
		if prev, dup := seen[leg["base"]]; dup {
			t.Errorf("pg%s and pg%s share base digest %s", prev, leg["pg"], leg["base"])
		}
		seen[leg["base"]] = leg["pg"]
	}
	if !slices.Equal(majors, ciMajors) {
		t.Errorf("image build majors %q, want %q", majors, ciMajors)
	}
	if ff := job.Strategy.FailFast; ff == nil || *ff {
		t.Error("image build fail-fast is not false: one major's failure hides the others")
	}
	docker := readImageFile(t, "Dockerfile.pg")
	from := directiveLines(docker, "FROM")
	want := "FROM pgvector/pgvector:0.8.2-pg${PG_MAJOR}@${BASE_DIGEST}"
	if len(from) != 1 || from[0] != want {
		t.Errorf("Dockerfile.pg FROM %q, want exactly %q", from, want)
	}
	build := stepIndex(relJob{Steps: job.Steps}, "docker build",
		"--build-arg PG_MAJOR=${{ matrix.pg }}", "--build-arg BASE_DIGEST=${{ matrix.base }}")
	if build < 0 {
		t.Error("no build step passes PG_MAJOR and BASE_DIGEST from the matrix")
	}
}

// State transitions: a revision is published once. A build of a revision
// that exists does nothing, and an image is pushed only after the server
// it runs passed the verifier.
func TestCIImageBuildNeverOverwritesARevision(t *testing.T) {
	job := loadImageBuild(t)
	rj := relJob{Steps: job.Steps}
	check := stepIndex(rj, "docker manifest inspect")
	build := stepIndex(rj, "docker build")
	verify := stepIndex(rj, ciVerify)
	push := stepIndex(rj, "docker push")
	if check < 0 || build < 0 || verify < 0 || push < 0 {
		t.Fatalf("steps: check %d, build %d, verify %d, push %d; want all four",
			check, build, verify, push)
	}
	if check != 1 || !(check < build && build < verify && verify < push) {
		t.Errorf("step order check %d, build %d, verify %d, push %d; want login, "+
			"check, build, verify, push", check, build, verify, push)
	}
	if job.Steps[check].ID != "published" {
		t.Errorf("the existence check has id %q, want published", job.Steps[check].ID)
	}
	// Fail closed: only a registry "not found" means unpublished. Any other
	// error (auth, rate limit, outage) fails the build instead of
	// rebuilding and overwriting a published revision.
	if run := job.Steps[check].Run; !strings.Contains(run, "no such manifest") ||
		!strings.Contains(run, "exit 1") {
		t.Errorf("the existence check treats every error as unpublished: %q", run)
	}
	gate := "steps.published.outputs.exists != 'true'"
	for i := check + 1; i < len(job.Steps); i++ {
		if !strings.Contains(expr(job.Steps[i].If), gate) {
			t.Errorf("step %d (%s) has if %q, want it gated on %s", i,
				job.Steps[i].Name, job.Steps[i].If, gate)
		}
	}
	rev := ciImageRevision(t, job)
	tag := "ghcr.io/${{ github.repository_owner }}/pg_sage-ci-pgvector:0.8.2-pg" +
		"${{ matrix.pg }}-" + rev
	if job.Env["TAG"] != tag {
		t.Errorf("build TAG = %q, want %q", job.Env["TAG"], tag)
	}
	for _, i := range []int{check, build, push} {
		if !strings.Contains(job.Steps[i].Run, `"$TAG"`) {
			t.Errorf("step %s does not use \"$TAG\": %q", job.Steps[i].Name, job.Steps[i].Run)
		}
	}
}

// parseCIConf reads the postgresql.conf lines the image appends: one
// name = value per line, # comments, values optionally single-quoted.
func parseCIConf(body string) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" || strings.ContainsAny(name, " \t'") {
			return nil, fmt.Errorf("line %d %q is not name = value", i+1, line)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("line %d sets %s twice", i+1, name)
		}
		out[name] = strings.Trim(value, "'")
	}
	return out, nil
}

func TestParseCIConf(t *testing.T) {
	got, err := parseCIConf("# c\n\nwal_level = logical  # x\r\n" +
		"shared_preload_libraries = 'a,b'\nx.max=5\n")
	want := map[string]string{"wal_level": "logical", "shared_preload_libraries": "a,b",
		"x.max": "5"}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("parseCIConf = %v, %v; want %v", got, err, want)
	}
	if got, err := parseCIConf(""); err != nil || len(got) != 0 {
		t.Fatalf("empty conf = %v, %v; want no settings", got, err)
	}
	for _, bad := range []string{"wal_level logical", "= 1", "a =", "a b = 1",
		"a = 1\na = 2"} {
		if _, err := parseCIConf(bad); err == nil || !strings.Contains(err.Error(), "line") {
			t.Errorf("parseCIConf(%q) err %v, want one naming the line", bad, err)
		}
	}
}

// The image carries what scripts/ci/setup-test-postgres.sh applied by hand
// before ci-03b (with two restarts per job), plus the connection limit the
// parallel suites need (r2), and nothing else.
func TestCIImageBakesTheTestServerSettings(t *testing.T) {
	conf, err := parseCIConf(readImageFile(t, "postgresql.ci.conf"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"shared_preload_libraries":  "pg_stat_statements,pg_hint_plan",
		"wal_level":                 "logical",
		"max_prepared_transactions": "10",
		"pg_stat_statements.max":    "50000",
		"max_connections":           "300",
	}
	if !maps.Equal(conf, want) {
		t.Errorf("postgresql.ci.conf = %v, want %v", conf, want)
	}
	docker := readImageFile(t, "Dockerfile.pg")
	for _, pkg := range []string{"hypopg", "pg-hint-plan", "pgaudit"} {
		if !strings.Contains(docker, "postgresql-${PG_MAJOR}-"+pkg) {
			t.Errorf("Dockerfile.pg does not install postgresql-${PG_MAJOR}-%s", pkg)
		}
	}
	if !strings.Contains(docker, ">> /usr/share/postgresql/postgresql.conf.sample") {
		t.Error("Dockerfile.pg does not append postgresql.ci.conf to the conf sample initdb uses")
	}
	sql := readImageFile(t, "initdb-ci.sql")
	if !strings.Contains(sql, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements") ||
		!strings.Contains(docker, "/docker-entrypoint-initdb.d/") {
		t.Error("the image does not create pg_stat_statements at initdb")
	}
	verify := readRepoFile(t, filepath.Join(repoRoot(t), ciVerify))
	for _, check := range []string{"postgresql.ci.conf", "CREATE EXTENSION hypopg",
		"pg_hint_plan.enable_hint", "pgaudit", "server_version_num"} {
		if !strings.Contains(verify, check) {
			t.Errorf("%s does not check %s", ciVerify, check)
		}
	}
}

// ciImageFiles are the files that may pull the CI image: every workflow
// but the build, and the CI scripts.
func ciImageFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range workflowFiles(t) {
		if filepath.Base(p) != ciImageBuild {
			out = append(out, p)
		}
	}
	scripts, err := filepath.Glob(filepath.Join(repoRoot(t), "scripts", "ci", "*.sh"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("list scripts/ci: %v (found %d)", err, len(scripts))
	}
	return append(out, scripts...)
}

// Every pull of the CI image names the revision the build publishes, so no
// job runs a plain mirror without the baked extensions.
func TestEveryCIImagePullIsTheBakedRevision(t *testing.T) {
	rev := ciImageRevision(t, loadImageBuild(t))
	pulls := 0
	for _, path := range ciImageFiles(t) {
		body := readRepoFile(t, path)
		name := filepath.Base(path)
		refs := ciImageRef.FindAllStringSubmatch(body, -1)
		if n := strings.Count(body, "pg_sage-ci-pgvector:"); n != len(refs) {
			t.Errorf("%s: %d CI image references, %d of the 0.8.2-pg<major> form",
				name, n, len(refs))
		}
		for _, m := range refs {
			pulls++
			if m[2] != "-"+rev {
				t.Errorf("%s pulls %s, want revision -%s", name, m[0], rev)
			}
		}
		if strings.HasSuffix(name, ".yml") && strings.Contains(body, "pgvector/pgvector:") {
			t.Errorf("%s pulls pgvector/pgvector from Docker Hub, want %s", name, ciImageRepo)
		}
	}
	if pulls < 10 {
		t.Errorf("found %d CI image pulls, want every server CI starts (10 or more)", pulls)
	}
}

// No job installs packages into its server or restarts it: the image has
// both, and the verifier only checks.
func TestNoCIJobPreparesPostgresByHand(t *testing.T) {
	old := filepath.Join(repoRoot(t), "scripts", "ci", "setup-test-postgres.sh")
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scripts/ci/setup-test-postgres.sh still exists (stat err %v)", err)
	}
	for _, path := range ciImageFiles(t) {
		body := readRepoFile(t, path)
		for _, banned := range []string{"apt-get", "setup-test-postgres.sh"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s still runs %s", filepath.Base(path), banned)
			}
		}
	}
}

type svcJob struct {
	Services struct {
		Postgres struct {
			Image string `yaml:"image"`
		} `yaml:"postgres"`
	} `yaml:"services"`
	Steps []relStep `yaml:"steps"`
}

// verifyMajor is the major a job's verify step passes, or "".
var verifyMajor = regexp.MustCompile(`verify-test-postgres\.sh \$\{\{ job\.services\.postgres` +
	`\.id \}\} ([0-9]{2}|\$\{\{ matrix\.pg \}\})`)

// Every job with a PostgreSQL service checks it before its first go test,
// with the major its image has.
func TestEveryServiceIsVerifiedBeforeItsTests(t *testing.T) {
	jobs := 0
	for _, path := range workflowFiles(t) {
		var wf struct {
			Jobs map[string]svcJob `yaml:"jobs"`
		}
		if err := yaml.Unmarshal([]byte(readRepoFile(t, path)), &wf); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for name, job := range wf.Jobs {
			img := ciImageRef.FindStringSubmatch(job.Services.Postgres.Image)
			if img == nil {
				continue
			}
			jobs++
			verify, firstTest := -1, -1
			for i, st := range job.Steps {
				if m := verifyMajor.FindStringSubmatch(st.Run); m != nil && verify < 0 {
					verify = i
					if m[1] != img[1] {
						t.Errorf("%s: verifies major %s, the service runs %s", name, m[1], img[1])
					}
				}
				if strings.Contains(st.Run, "go test ") && firstTest < 0 {
					firstTest = i
				}
			}
			if verify < 0 || (firstTest >= 0 && firstTest < verify) {
				t.Errorf("%s: verify step %d, first go test %d; want the server verified first",
					name, verify, firstTest)
			}
		}
	}
	if jobs < 7 {
		t.Errorf("found %d jobs with a CI PostgreSQL service, want test, integration-matrix, "+
			"bench, safety-bench, perfgate, shuffle and bench-live at least", jobs)
	}
}
