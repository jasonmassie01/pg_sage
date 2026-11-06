// Command baseline rewrites the AgentSafetyBench baseline
// (safety-bench/testdata/baseline.json) from a bench report. Run it from
// sidecar/ with the report of a cgo build on the PG17 reference server,
// such as the agentsafetybench-pg17 CI artifact:
//
//	go run ./safety-bench/cmd/baseline -report agentsafetybench.json
//
// Existing pg_overrides entries (per-major differences, each with a
// reason) are kept as they are.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	safetybench "github.com/pg-sage/sidecar/safety-bench"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("baseline", flag.ContinueOnError)
	flags.SetOutput(stderr)
	report := flags.String("report", "",
		"agentsafetybench.json from a cgo build on PostgreSQL 17")
	out := flags.String("out", filepath.Join("safety-bench", safetybench.BaselineFile),
		"baseline file to write")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *report == "" {
		_, _ = fmt.Fprintln(stderr, "baseline: -report <agentsafetybench.json> is required")
		return 2
	}
	if err := regenerate(*report, *out); err != nil {
		_, _ = fmt.Fprintf(stderr, "baseline: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "wrote %s from %s\n", *out, *report)
	return 0
}

// regenerate writes out from the report, keeping out's pg_overrides. A
// missing out starts with none; an unreadable one stops the run untouched.
func regenerate(reportPath, out string) error {
	r, err := safetybench.ReadReport(reportPath)
	if err != nil {
		return err
	}
	var keep []safetybench.PGOverride
	old, err := safetybench.LoadBaseline(out)
	switch {
	case err == nil:
		keep = old.PGOverrides
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	b, err := safetybench.BaselineFromReport(r, keep)
	if err != nil {
		return fmt.Errorf("%s: %w", reportPath, err)
	}
	return safetybench.WriteBaseline(out, b)
}
