// Command covfloor fails when a package's statement coverage in a
// `go test -cover -json` stream on stdin is under its floor (ci-20).
//
//	go test ./... -cover -json > ../unit.json
//	go run ./cmd/covfloor < ../unit.json
//
// Floors come from .coverage-floors (`<package-regex> <floor> # reason`,
// first match wins, patterns anchored); a package no line matches has the
// -default floor (70, CLAUDE.md's floor for business logic). A package that
// failed to build, or whose tests ran without printing a coverage line,
// fails. A package without tests has coverage 0.0% (go 1.22 and later), so
// it fails too unless a floor-0 line exempts it. Only a package with no
// statements ("[no statements]", or "[no test files]" with no coverage
// line) has nothing to cover and passes.
//
// Exit status: 0 every package meets its floor, 1 a package is under its
// floor or failed to build, 2 bad usage, a bad floors file, or a stream that
// is empty or has malformed lines.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
)

// defaultFloor is CLAUDE.md's coverage floor for business-logic packages.
const defaultFloor = 70.0

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type settings struct {
	floorsPath string
	def        float64
}

func parseFlags(args []string, stderr io.Writer) (settings, error) {
	fs := flag.NewFlagSet("covfloor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var s settings
	fs.StringVar(&s.floorsPath, "floors", ".coverage-floors", "coverage floors file")
	fs.Float64Var(&s.def, "default", defaultFloor,
		"floor, in percent, for packages no floors line matches")
	if err := fs.Parse(args); err != nil {
		return settings{}, err
	}
	if math.IsNaN(s.def) || s.def < 0 || s.def > 100 {
		return settings{}, fmt.Errorf("-default %v: want a percentage from 0 to 100", s.def)
	}
	return s, nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	s, err := parseFlags(args, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "covfloor: %v\n", err)
		return 2
	}
	rules, err := loadFloors(s.floorsPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "covfloor: %v\n", err)
		return 2
	}
	st, err := readStream(stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "covfloor: read the go test -json stream: %v\n", err)
		return 2
	}
	if len(st.packages) == 0 && len(st.malformed) == 0 {
		_, _ = fmt.Fprintln(stderr, "covfloor: no test output on stdin: "+
			"pipe in a go test -cover -json stream")
		return 2
	}
	rep := evaluate(st, rules, s.def)
	_, _ = fmt.Fprint(stdout, formatReport(rep, s.floorsPath, s.def))
	if len(st.malformed) > 0 {
		_, _ = fmt.Fprintf(stderr, "covfloor: %s\n", malformedNote(st))
		return 2
	}
	if len(rep.failures) > 0 {
		return 1
	}
	return 0
}

func loadFloors(path string) ([]floorRule, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open the floors file: %w", err)
	}
	defer func() { _ = file.Close() }()
	rules, err := parseFloors(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rules, nil
}
