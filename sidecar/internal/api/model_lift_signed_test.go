package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/earned"
)

// benches-09: only a signed release or local-run lift report stamped with
// the running build earns model-root authority, so the model-lift tests
// run their ledgers as a release build and ingest signed reports.

const uploadCommit = "cccccccccccccccccccccccccccccccccccccccc"

var uploadBuild = earned.Build{Version: "1.8.5", Commit: uploadCommit}

// useReleaseBuild rebinds orders to a ledger running uploadBuild.
func (f *autonomyAPIFixture) useReleaseBuild() {
	f.t.Helper()
	f.build = uploadBuild
	f.ledger = f.register("orders")
}

// ingestSignedLift ingests raw stamped with uploadBuild as a signed
// release report: since benches-09 only a signed or local-run, stamped
// lift counts. The fixture runs useReleaseBuild first.
func (f *autonomyAPIFixture) ingestSignedLift(raw string) earned.EvalRun {
	f.t.Helper()
	stamped := strings.Replace(raw, `"generated_at"`, fmt.Sprintf(
		`"pg_sage_version": %q, "pg_sage_commit": %q, "generated_at"`, uploadBuild.Version,
		uploadBuild.Commit), 1)
	run, err := f.ledger.IngestBench(context.Background(), []byte(stamped),
		earned.BenchIngest{Origin: earned.OriginSignedRelease, Actor: "release_bench",
			RequireBuild: true, Signature: &earned.ReportSignature{
				Identity: "ci", Issuer: benchsig.GitHubIssuer, Commit: uploadCommit,
				SignedAt: time.Now().UTC()}})
	if err != nil {
		f.t.Fatalf("ingest a signed lift report: %v", err)
	}
	return run
}
