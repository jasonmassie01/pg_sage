// Command skipbudget fails when a `go test -json` stream on stdin contains
// a skipped test that the allow-list does not justify.
//
//	go test -json ./... | go run ./cmd/skipbudget -allow .skip-allowlist
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pg-sage/sidecar/internal/testsupport/skipbudget"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("skipbudget", flag.ContinueOnError)
	fs.SetOutput(stderr)
	allowPath := fs.String("allow", ".skip-allowlist", "allow-list file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	file, err := os.Open(*allowPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "skipbudget: open allow-list: %v\n", err)
		return 2
	}
	defer func() { _ = file.Close() }()
	rules, err := skipbudget.ParseRules(file)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "skipbudget: %v\n", err)
		return 2
	}
	report, err := skipbudget.Check(stdin, rules)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "skipbudget: %v\n", err)
		return 2
	}
	_, _ = fmt.Fprint(stdout, skipbudget.Format(report))
	if len(report.Violations) > 0 {
		return 1
	}
	return 0
}
