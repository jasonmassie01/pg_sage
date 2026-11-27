package startup

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Release toolchain contract (ci-01): the toolchain line in sidecar/go.mod
// is the one source of truth for the Go release that builds every shipped
// artifact. Both Dockerfiles' golang stages and the goreleaser-cross image
// must name that exact patch release, runtime Alpine stages are pinned by
// digest, and Node is a supported LTS line (22 or newer).
//
// No concurrency, state-transition or PostgreSQL surface: these tests read
// repository build files and hold no shared state.

const minNodeMajor = 22

var (
	toolchainLine = regexp.MustCompile(`^toolchain\s+go(\d+\.\d+\.\d+)\s*(//.*)?$`)
	goLine        = regexp.MustCompile(`^go\s+(\d+\.\d+(?:\.\d+)?)\s*(//.*)?$`)
	fromLine      = regexp.MustCompile(`(?i)^FROM\s+(?:--platform=\S+\s+)?(\S+)`)
	// A docker run (its continued lines up to the image) and the
	// goreleaser-cross image it runs.
	crossImage = regexp.MustCompile(
		`(docker run(?:[^\n]*\\\n)*?)[^\n]*ghcr\.io/goreleaser/goreleaser-cross:([^\s'"]+)`)
	nodeVersion  = regexp.MustCompile(`(?m)^\s*node-version:\s*["']?([^"'\s]*)`)
	literalGo    = regexp.MustCompile(`(?m)^\s*go-version:\s`)
	digestSuffix = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	leadingMajor = regexp.MustCompile(`^v?(\d+)(?:[.-]|$)`)
)

// shippedDockerfiles are the Dockerfiles that build released artifacts.
func shippedDockerfiles(t *testing.T) []string {
	t.Helper()
	root := wave5SidecarRoot(t)
	return []string{
		filepath.Join(root, "Dockerfile"),
		filepath.Join(root, "..", "deploy", "databricks-apps", "Dockerfile"),
	}
}

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// directiveLines returns the uncommented lines of body whose first field is
// keyword.
func directiveLines(body, keyword string) []string {
	var out []string
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == keyword {
			out = append(out, line)
		}
	}
	return out
}

// parseToolchain returns the version (without the "go" prefix) of the
// single toolchain line in a go.mod body named name.
func parseToolchain(name, body string) (string, error) {
	lines := directiveLines(body, "toolchain")
	switch len(lines) {
	case 0:
		return "", fmt.Errorf(
			"%s has no toolchain line; add toolchain go<major>.<minor>.<patch>", name)
	case 1:
	default:
		return "", fmt.Errorf("%s has %d toolchain lines, want exactly one", name, len(lines))
	}
	m := toolchainLine.FindStringSubmatch(lines[0])
	if m == nil {
		return "", fmt.Errorf("%s: malformed %q; want toolchain go<major>.<minor>.<patch>",
			name, lines[0])
	}
	return m[1], nil
}

// parseGoDirective returns the version of the single go line of a go.mod.
func parseGoDirective(name, body string) (string, error) {
	lines := directiveLines(body, "go")
	if len(lines) != 1 {
		return "", fmt.Errorf("%s has %d go lines, want exactly one", name, len(lines))
	}
	m := goLine.FindStringSubmatch(lines[0])
	if m == nil {
		return "", fmt.Errorf("%s: malformed %q", name, lines[0])
	}
	return m[1], nil
}

// dockerStages returns the image reference of every FROM line.
func dockerStages(body string) []string {
	var out []string
	for _, raw := range strings.Split(body, "\n") {
		if m := fromLine.FindStringSubmatch(strings.TrimSpace(raw)); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// imageName is the repository part of an image reference.
func imageName(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	name, _, _ = strings.Cut(name, ":")
	return name
}

// imageTag is the tag of an image reference ("" when it has none).
func imageTag(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	_, tag, _ := strings.Cut(name, ":")
	return tag
}

// checkGolangImage fails unless ref is a golang image of exactly version,
// optionally with a variant suffix such as -alpine or -alpine3.24.
func checkGolangImage(ref, version string) error {
	tag := imageTag(ref)
	if tag == version || strings.HasPrefix(tag, version+"-") {
		return nil
	}
	return fmt.Errorf("%s does not match go.mod toolchain go%s (want golang:%s-alpine)",
		ref, version, version)
}

// checkCrossTag fails unless a goreleaser-cross tag names exactly version
// (a "-N" image rebuild of the same Go release is accepted).
func checkCrossTag(tag, version string) error {
	want := "v" + version
	if tag == want || strings.HasPrefix(tag, want+"-") {
		return nil
	}
	return fmt.Errorf("goreleaser-cross:%s does not match go.mod toolchain go%s (want :%s)",
		tag, version, want)
}

// checkCrossRun accepts a goreleaser-cross image of exactly version, or,
// while goreleaser-cross has not published that Go release yet, an older
// patch of the same minor whose docker run sets GOTOOLCHAIN=go<version>,
// so the release binaries are still built by go.mod's toolchain.
func checkCrossRun(runLine, tag, version string) error {
	err := checkCrossTag(tag, version)
	if err == nil {
		return nil
	}
	if !olderPatchOfMinor(tag, version) {
		return err
	}
	if !strings.Contains(runLine, "-e GOTOOLCHAIN=go"+version+" ") {
		return fmt.Errorf("goreleaser-cross:%s is older than go.mod toolchain go%s and its "+
			"docker run does not set -e GOTOOLCHAIN=go%s", tag, version, version)
	}
	return nil
}

// olderPatchOfMinor reports whether tag is v<major>.<minor>.<p> (or a -N
// rebuild of it) with p below version's patch, in the same major.minor.
func olderPatchOfMinor(tag, version string) bool {
	vMaj, vMin, vPatch, ok := splitVersion(version)
	if !ok {
		return false
	}
	base, _, _ := strings.Cut(strings.TrimPrefix(tag, "v"), "-")
	if !strings.HasPrefix(tag, "v") {
		return false
	}
	maj, min, patch, ok := splitVersion(base)
	return ok && maj == vMaj && min == vMin && patch < vPatch
}

func splitVersion(v string) (major, minor, patch int, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// checkDigestPinned fails unless ref ends in a full sha256 digest.
func checkDigestPinned(ref string) error {
	if digestSuffix.MatchString(ref) {
		return nil
	}
	return fmt.Errorf("%s is not pinned by @sha256:<64 hex> digest", ref)
}

// checkNodeMajor fails unless version starts with a major of at least 22.
func checkNodeMajor(source, version string) error {
	m := leadingMajor.FindStringSubmatch(version)
	if m == nil {
		return fmt.Errorf("%s: node version %q is not a numbered release", source, version)
	}
	major, err := strconv.Atoi(m[1])
	if err != nil {
		return fmt.Errorf("%s: node version %q: %v", source, version, err)
	}
	if major < minNodeMajor {
		return fmt.Errorf("%s: node %s is below the supported LTS line %d",
			source, version, minNodeMajor)
	}
	return nil
}

// minorOf returns the minor number of a major.minor[.patch] Go version.
func minorOf(t *testing.T, version string) int {
	t.Helper()
	parts := strings.Split(version, ".")
	if len(parts) < 2 || parts[0] != "1" {
		t.Fatalf("Go version %q is not 1.<minor>[.<patch>]", version)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		t.Fatalf("Go version %q: %v", version, err)
	}
	return minor
}

func goModToolchain(t *testing.T) string {
	t.Helper()
	path := filepath.Join(wave5SidecarRoot(t), "go.mod")
	version, err := parseToolchain(path, readRepoFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func ciWorkflowPath(t *testing.T) string {
	return filepath.Join(wave5SidecarRoot(t), "..", ".github", "workflows", "ci.yml")
}

func workflowFiles(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(wave5SidecarRoot(t), "..", ".github", "workflows")
	paths, err := filepath.Glob(filepath.Join(dir, "*.yml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("list workflows in %s: %v (found %d)", dir, err, len(paths))
	}
	return paths
}

func TestToolchainPinsAgree(t *testing.T) {
	version := goModToolchain(t)
	for _, path := range shippedDockerfiles(t) {
		golang := 0
		for _, ref := range dockerStages(readRepoFile(t, path)) {
			if imageName(ref) != "golang" {
				continue
			}
			golang++
			if err := checkGolangImage(ref, version); err != nil {
				t.Errorf("%s: %v", path, err)
			}
		}
		if golang == 0 {
			t.Errorf("%s has no golang build stage", path)
		}
	}
	path := ciWorkflowPath(t)
	tags := crossImage.FindAllStringSubmatch(readRepoFile(t, path), -1)
	if len(tags) == 0 {
		t.Fatalf("%s names no goreleaser-cross image", path)
	}
	for _, m := range tags {
		if err := checkCrossRun(m[1], m[2], version); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

// The go line is the minimum for contributors: it must not name a newer
// release than the toolchain, and it must be a supported line (Go supports
// the two newest minor releases).
func TestToolchainGoDirectiveIsSupportedLine(t *testing.T) {
	path := filepath.Join(wave5SidecarRoot(t), "go.mod")
	goVersion, err := parseGoDirective(path, readRepoFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	toolchain := goModToolchain(t)
	goMinor, toolMinor := minorOf(t, goVersion), minorOf(t, toolchain)
	if goMinor > toolMinor {
		t.Errorf("go %s is newer than toolchain go%s", goVersion, toolchain)
	}
	if goMinor < toolMinor-1 {
		t.Errorf("go %s is below the supported lines of toolchain go%s (want 1.%d or 1.%d)",
			goVersion, toolchain, toolMinor-1, toolMinor)
	}
}

func TestToolchainRuntimeAlpineIsDigestPinned(t *testing.T) {
	sidecarAlpine := 0
	for i, path := range shippedDockerfiles(t) {
		for _, ref := range dockerStages(readRepoFile(t, path)) {
			if imageName(ref) != "alpine" {
				continue
			}
			if i == 0 {
				sidecarAlpine++
			}
			if err := checkDigestPinned(ref); err != nil {
				t.Errorf("%s: %v", path, err)
			}
		}
	}
	if sidecarAlpine == 0 {
		t.Error("sidecar/Dockerfile has no alpine runtime stage to check")
	}
}

func TestToolchainNodeIsSupportedLTS(t *testing.T) {
	for _, path := range shippedDockerfiles(t) {
		found := 0
		for _, ref := range dockerStages(readRepoFile(t, path)) {
			if imageName(ref) != "node" {
				continue
			}
			found++
			if err := checkNodeMajor(path, imageTag(ref)); err != nil {
				t.Error(err)
			}
		}
		if found == 0 {
			t.Errorf("%s has no node frontend stage", path)
		}
	}
	setupNode := 0
	for _, path := range workflowFiles(t) {
		for _, m := range nodeVersion.FindAllStringSubmatch(readRepoFile(t, path), -1) {
			if filepath.Base(path) == "ci.yml" {
				setupNode++
			}
			if err := checkNodeMajor(path, m[1]); err != nil {
				t.Error(err)
			}
		}
	}
	if setupNode < 2 {
		t.Errorf("ci.yml has %d node-version pins, want the test and release setup-node", setupNode)
	}
}

// One source of truth: no workflow may pin a literal go-version beside
// go.mod; every setup-go reads sidecar/go.mod.
func TestToolchainSetupGoReadsGoMod(t *testing.T) {
	for _, path := range workflowFiles(t) {
		body := readRepoFile(t, path)
		if literalGo.MatchString(body) {
			t.Errorf("%s pins a literal go-version; use go-version-file: sidecar/go.mod", path)
		}
		uses := strings.Count(body, "actions/setup-go@")
		files := strings.Count(body, "go-version-file: sidecar/go.mod")
		if uses != files {
			t.Errorf("%s: %d setup-go steps but %d read sidecar/go.mod", path, uses, files)
		}
	}
}
