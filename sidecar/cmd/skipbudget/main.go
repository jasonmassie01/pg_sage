// Command skipbudget fails when a `go test -json` stream on stdin contains
// a skipped test that the allow-list does not justify on this server major,
// when a -require'd test did not pass, or (with -stale=fail) when an
// allow-list rule matched nothing.
//
//	go test -json ./... | go run ./cmd/skipbudget -allow .skip-allowlist -pg 17 \
//	    -require 'internal/notify .*'
//
// Exit status: 0 clean, 1 budget violated, 2 bad usage or unreadable input.
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

// requireFlag collects repeated -require specs.
type requireFlag []skipbudget.Requirement

func (f *requireFlag) String() string { return fmt.Sprint(len(*f)) }

func (f *requireFlag) Set(spec string) error {
	req, err := skipbudget.ParseRequirement(spec)
	if err != nil {
		return err
	}
	*f = append(*f, req)
	return nil
}

type settings struct {
	allowPath string
	stale     string
	opts      skipbudget.Options
}

func parseFlags(args []string, stderr io.Writer) (settings, error) {
	fs := flag.NewFlagSet("skipbudget", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var s settings
	var require requireFlag
	fs.StringVar(&s.allowPath, "allow", ".skip-allowlist", "allow-list file")
	fs.IntVar(&s.opts.PG, "pg", 0,
		"server major the stream ran on (required when a rule is pg-qualified)")
	fs.Var(&require, "require",
		"'<package-re> <test-re>' that must pass at least once (repeatable)")
	fs.StringVar(&s.stale, "stale", "warn",
		"rules that matched no skip: warn, fail, or off (for a partial -run stream)")
	if err := fs.Parse(args); err != nil {
		return settings{}, err
	}
	if s.opts.PG < 0 {
		return settings{}, fmt.Errorf("-pg %d: want a positive server major", s.opts.PG)
	}
	if s.stale != "warn" && s.stale != "fail" && s.stale != "off" {
		return settings{}, fmt.Errorf("-stale=%s: want warn, fail or off", s.stale)
	}
	s.opts.Require = require
	return s, nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	s, err := parseFlags(args, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "skipbudget: %v\n", err)
		return 2
	}
	file, err := os.Open(s.allowPath)
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
	report, err := skipbudget.Check(stdin, rules, s.opts)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "skipbudget: %v\n", err)
		return 2
	}
	if s.stale == "off" {
		report.Stale = nil
	}
	_, _ = fmt.Fprint(stdout, skipbudget.Format(report))
	if len(report.Violations) > 0 || len(report.Unmet) > 0 {
		return 1
	}
	if len(report.Stale) > 0 {
		if s.stale == "fail" {
			return 1
		}
		_, _ = fmt.Fprintf(stderr, "skipbudget: warning: %d allow-list rules matched "+
			"no skip; delete or fix them (-stale=fail will reject them)\n", len(report.Stale))
	}
	return 0
}
