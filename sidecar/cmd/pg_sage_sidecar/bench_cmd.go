package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/earned"
)

// pg_sage bench verify [--commit SHA] <report.json>: verifies a
// PGIncidentBench report's Sigstore bundle (<report.json>.sigstore.json)
// offline, against the trusted root embedded in this binary, as the
// pg_sage release workflow's signature. The release workflow runs it on
// the reports it just signed; an operator can run it on downloaded
// release assets.

const benchUsage = "usage: pg_sage bench verify [--commit SHA] <pgincidentbench.json>\n" +
	"       pg_sage bench export-replay --investigation ID [--keep-identifiers] [--out FILE]"

func runBenchCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "export-replay" {
		return runExportReplay(context.Background(), args[1:], os.Getenv, stdout, stderr)
	}
	if len(args) == 0 || args[0] != "verify" {
		_, _ = fmt.Fprintln(stderr, benchUsage)
		return 2
	}
	fs := flag.NewFlagSet("bench verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	want := fs.String("commit", "", "the commit the report must have been built from")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, benchUsage)
		return 2
	}
	sig, err := verifyReportFile(fs.Arg(0), *want)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "bench verify %s: %v\n", fs.Arg(0), err)
		return 1
	}
	commitText := sig.Commit
	if commitText == "" {
		commitText = "not named"
	}
	_, _ = fmt.Fprintf(stdout, "%s: signed by %s (issuer %s), commit %s, signed at %s\n",
		fs.Arg(0), sig.Identity, sig.Issuer, commitText, sig.SignedAt.Format(time.RFC3339))
	return 0
}

// checkReportCommit holds the report's pg_sage_commit and the
// certificate's commit to each other and, when set, to want.
func checkReportCommit(raw []byte, sig benchsig.Signature, want string) error {
	var stamp struct {
		Commit string `json:"pg_sage_commit"`
	}
	if err := json.Unmarshal(raw, &stamp); err != nil {
		return fmt.Errorf("read the report's pg_sage_commit: %w", err)
	}
	report := earned.Build{Commit: stamp.Commit}.Normalized().Commit
	signed := strings.ToLower(sig.Commit)
	if signed != "" && report != "" && signed != report {
		return fmt.Errorf("the signature names commit %s, the report %s", signed, report)
	}
	want = earned.Build{Commit: want}.Normalized().Commit
	if want == "" {
		return nil
	}
	for _, got := range []string{signed, report} {
		if got != "" && got != want {
			return fmt.Errorf("built from commit %s, not %s", got, want)
		}
	}
	if signed == "" && report == "" {
		return fmt.Errorf("neither the signature nor the report names a commit (want %s)",
			want)
	}
	return nil
}
