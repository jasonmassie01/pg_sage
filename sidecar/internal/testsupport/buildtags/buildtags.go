// Package buildtags checks that CI compiles every tagged test suite: it
// collects the custom build tags used in the module and compares them with
// the -tags= list of CI's compile-only vet step (ci-04).
package buildtags

import (
	"bufio"
	"bytes"
	"fmt"
	"go/build/constraint"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// platformTags are the tags the toolchain sets itself (GOOS, GOARCH,
// cgo, compiler, unix, ignore, and race, which -race sets); a vet step cannot
// and need not list them.
var platformTags = map[string]bool{
	"cgo": true, "unix": true, "ignore": true, "gc": true, "gccgo": true, "race": true,
	// GOOS values (go/build/syslist.go).
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "nacl": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,
	// GOARCH values (go/build/syslist.go).
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "armbe": true,
	"arm64": true, "arm64be": true, "loong64": true, "mips": true,
	"mipsle": true, "mips64": true, "mips64le": true, "mips64p32": true,
	"mips64p32le": true, "ppc": true, "ppc64": true, "ppc64le": true,
	"riscv": true, "riscv64": true, "s390": true, "s390x": true,
	"sparc": true, "sparc64": true, "wasm": true,
}

var releaseTag = regexp.MustCompile(`^go1\.\d+$`)

// vetStep matches a go vet invocation with -tags=list or -tags list.
var vetStep = regexp.MustCompile(`go vet\s+-tags[=\s]([A-Za-z0-9_.,]+)`)

// CustomTags returns the sorted custom tags of one //go:build or
// // +build line, excluding platform, release and toolchain tags.
func CustomTags(line string) ([]string, error) {
	expr, err := constraint.Parse(line)
	if err != nil {
		return nil, fmt.Errorf("parse build constraint %q: %w", line, err)
	}
	var tags []string
	expr.Eval(func(tag string) bool {
		if !platformTags[tag] && !releaseTag.MatchString(tag) &&
			!strings.HasPrefix(tag, "goexperiment.") && !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
		return false
	})
	slices.Sort(tags)
	return tags, nil
}

// ScanTree maps each custom tag used under root to the files that use it.
// Like the go tool's ./..., it skips testdata, vendor, node_modules, dot
// or underscore directories and nested modules (a directory with its own
// go.mod), and reads constraints only above the package clause.
func ScanTree(root string) (map[string][]string, error) {
	tags := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}
		if d.IsDir() {
			if path != root && (skipDir(d.Name()) || isModuleRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		found, err := fileTags(path)
		for _, tag := range found {
			tags[tag] = append(tags[tag], path)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return tags, nil
}

func skipDir(name string) bool {
	return name == "testdata" || name == "vendor" || name == "node_modules" ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// isModuleRoot reports whether dir holds a go.mod: its packages belong to
// another module, which the module's go vet ./... does not compile.
func isModuleRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

func fileTags(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var tags []string
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) && !constraint.IsPlusBuild(line) {
			continue
		}
		found, err := CustomTags(line)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		tags = append(tags, found...)
	}
	return tags, sc.Err()
}

// VetTags returns the -tags list of the first go vet step in a workflow.
func VetTags(workflow []byte) ([]string, error) {
	m := vetStep.FindSubmatch(workflow)
	if m == nil {
		return nil, fmt.Errorf("no tagged vet step (go vet -tags=...) in the CI workflow")
	}
	var tags []string
	for _, tag := range strings.Split(string(m[1]), ",") {
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("no tagged vet step: go vet -tags lists no tag")
	}
	return tags, nil
}

// MissingTags returns, sorted, the used tags that vetted does not list.
func MissingTags(used map[string][]string, vetted []string) []string {
	var missing []string
	for tag := range used {
		if !slices.Contains(vetted, tag) {
			missing = append(missing, tag)
		}
	}
	slices.Sort(missing)
	return missing
}
