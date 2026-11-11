// Command gen_metrics_doc renders docs/metrics.md, the metrics reference,
// from internal/metricsreg and the alert runbooks (observability-09). It
// is a build tool, not part of the sidecar binary; go generate runs it
// from internal/metricsreg.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/pg-sage/sidecar/internal/metricsreg"
)

// errDrift is returned by -check when the file differs from the render.
var errDrift = errors.New("is out of date; run `go generate ./internal/metricsreg/` " +
	"in sidecar/")

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gen_metrics_doc:", err)
		os.Exit(1)
	}
}

// run writes the reference to -out, or with -check only reports drift.
func run(args []string) error {
	fs := flag.NewFlagSet("gen_metrics_doc", flag.ContinueOnError)
	out := fs.String("out", "../docs/metrics.md", "the metrics reference to write")
	check := fs.Bool("check", false, "report drift without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := metricsreg.Validate(metricsreg.Families(),
		metricsreg.RetiredFamilies()); err != nil {
		return fmt.Errorf("the metrics registry is invalid: %w", err)
	}
	want := []byte(render(metricsreg.Families(), metricsreg.Areas(),
		metricsreg.RetiredFamilies(), runbooks))
	if *check {
		got, err := os.ReadFile(*out)
		if err != nil {
			return fmt.Errorf("read %s: %w", *out, err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			return fmt.Errorf("%s %w", *out, errDrift)
		}
		return nil
	}
	if err := os.WriteFile(*out, want, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	return nil
}
