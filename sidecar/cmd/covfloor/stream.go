package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const modulePath = "github.com/pg-sage/sidecar"

// modulePrefix is stripped from package paths before floors are matched.
const modulePrefix = modulePath + "/"

var coverageLine = regexp.MustCompile(`coverage: ([0-9]+(?:\.[0-9]+)?)% of statements`)

// pkgResult is what the stream says about one package. Only package-level
// output counts: a test that prints "coverage: ..." cannot set the figure.
type pkgResult struct {
	ran          bool // a test of the package ran
	hasCoverage  bool
	coverage     float64
	noStatements bool // "coverage: [no statements]"
	noTestFiles  bool // "?   <pkg> [no test files]"
	buildFailed  bool
	failed       bool // the package's tests failed (a timeout prints no coverage)
}

type stream struct {
	packages  map[string]*pkgResult // keyed by path relative to the module root
	malformed []int                 // line numbers that are not go test events
}

type testEvent struct {
	Action      string
	Package     string
	Test        string
	Output      string
	FailedBuild string
}

// readStream scans a `go test -json` stream. A line that is not a JSON
// event with an Action is recorded as malformed, never skipped silently.
// Build events without a Package (build-output, build-fail) are ignored;
// the package's own "fail" event carries the build failure.
func readStream(r io.Reader) (stream, error) {
	st := stream{packages: map[string]*pkgResult{}}
	reader := bufio.NewReader(r)
	for line := 1; ; line++ {
		raw, err := reader.ReadString('\n')
		if text := strings.TrimSpace(raw); text != "" {
			st.add(line, text)
		}
		if errors.Is(err, io.EOF) {
			return st, nil
		}
		if err != nil {
			return stream{}, fmt.Errorf("line %d: %w", line, err)
		}
	}
}

func (st *stream) add(line int, text string) {
	var ev testEvent
	if err := json.Unmarshal([]byte(text), &ev); err != nil || ev.Action == "" {
		st.malformed = append(st.malformed, line)
		return
	}
	if ev.Package == "" {
		return
	}
	pkg := st.result(ev.Package)
	switch {
	case ev.Test != "":
		if ev.Action == "run" {
			pkg.ran = true
		}
	case ev.Action == "fail" && ev.FailedBuild != "":
		pkg.buildFailed = true
	case ev.Action == "fail":
		pkg.failed = true
	case ev.Action == "output":
		pkg.observe(ev.Output)
	}
}

func (st *stream) result(full string) *pkgResult {
	name := strings.TrimPrefix(full, modulePrefix)
	if full == modulePath {
		name = "."
	}
	pkg, ok := st.packages[name]
	if !ok {
		pkg = &pkgResult{}
		st.packages[name] = pkg
	}
	return pkg
}

// observe reads one package-level output line; the last coverage line wins.
func (p *pkgResult) observe(out string) {
	switch {
	case strings.Contains(out, "[build failed]"), strings.Contains(out, "[setup failed]"):
		p.buildFailed = true
	case strings.Contains(out, "coverage: [no statements]"):
		p.hasCoverage, p.noStatements = false, true
	case strings.Contains(out, "[no test files]"):
		p.noTestFiles = true
	}
	m := coverageLine.FindStringSubmatch(out)
	if m == nil {
		return
	}
	pct, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return // out of float range: not a coverage figure go test prints
	}
	p.hasCoverage, p.noStatements, p.coverage = true, false, pct
}

// malformedNote describes the malformed lines, naming the first ten.
func malformedNote(st stream) string {
	shown := st.malformed
	if len(shown) > 10 {
		shown = shown[:10]
	}
	nums := make([]string, len(shown))
	for i, n := range shown {
		nums[i] = strconv.Itoa(n)
	}
	more := ""
	if len(st.malformed) > len(shown) {
		more = ", ..."
	}
	return fmt.Sprintf("%d malformed lines in the go test -json stream (lines %s%s): "+
		"a package's coverage line may be among them, so the result cannot be trusted",
		len(st.malformed), strings.Join(nums, ", "), more)
}
