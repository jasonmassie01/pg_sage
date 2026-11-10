package benchingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/earned"
)

// earned-10: a report stamped for another pg_sage build is refused
// (earned.ErrBuildMismatch). After an upgrade every old report does that
// on every hourly tick, so a refusal is returned in Result.Refused (path
// and content hash) for the caller to tell once, not in the joined error
// it warns about on every tick. Every other failure stays an error.
//
// No concurrent-access test here: Ingest keeps no state of its own.

// byContentLedger fails the reports whose content it maps to an error.
type byContentLedger struct {
	fakeLedger
	errs    map[string]error
	refused []earned.BenchIngest
}

func (l *byContentLedger) IngestBench(ctx context.Context, raw []byte,
	in earned.BenchIngest) (earned.EvalRun, error) {
	if err := l.errs[string(raw)]; err != nil {
		l.refused = append(l.refused, in)
		return earned.EvalRun{}, err
	}
	return l.fakeLedger.IngestBench(ctx, raw, in)
}

func hexSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func mismatch() error {
	return fmt.Errorf("%w: it scores pg_sage 1.8.4, this is pg_sage 1.8.5",
		earned.ErrBuildMismatch)
}

func TestIngestRefusesAMismatchedBuildApartFromOtherErrors(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "core", "stale.json")
	broken := filepath.Join(dir, "reactive", "broken.json")
	good := filepath.Join(dir, "runway", "good.json")
	write(t, stale, "stale")
	write(t, broken, "broken")
	write(t, good, "good")
	ledger := &byContentLedger{errs: map[string]error{"stale": mismatch(),
		"broken": fmt.Errorf("%w: unexpected end of JSON", earned.ErrInvalidReport)}}
	res, err := Ingest(context.Background(), ledger, nil,
		Source{Path: dir, Actor: "bench_results_path"})
	if len(res.Refused) != 1 || res.Refused[0].Path != stale ||
		res.Refused[0].SHA256 != hexSHA("stale") ||
		!strings.Contains(res.Refused[0].Reason, "it scores pg_sage 1.8.4") {
		t.Fatalf("refused = %+v, want the stale report with its hash and build", res.Refused)
	}
	if !errors.Is(err, earned.ErrInvalidReport) || errors.Is(err, earned.ErrBuildMismatch) {
		t.Fatalf("err = %v, want only the malformed file's error", err)
	}
	if !strings.Contains(err.Error(), "broken.json") || strings.Contains(err.Error(), "stale") {
		t.Fatalf("the joined error must name only the malformed file: %v", err)
	}
	if res.Files != 3 || res.Added != 1 || len(res.Skipped) != 0 {
		t.Fatalf("result = %+v, want 3 files read and the good one added", res)
	}
}

func TestIngestOnlyRefusedReportsIsNoError(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	write(t, report, "old build")
	ledger := &byContentLedger{errs: map[string]error{"old build": mismatch()}}
	for name, path := range map[string]string{"directory": dir, "single file": report} {
		res, err := Ingest(context.Background(), ledger, nil,
			Source{Path: path, Actor: "bench_results_path"})
		if err != nil || len(res.Refused) != 1 || res.Refused[0].Path != report ||
			res.Refused[0].SHA256 != hexSHA("old build") || res.Added != 0 || res.Files != 1 {
			t.Fatalf("%s: ingest = %+v (%v), want one refusal and no error", name, res, err)
		}
	}
}

// A shipped report must name the running build: the ledger refuses an
// unstamped one with ErrBuildMismatch too, and it is a refusal.
func TestIngestRefusesAnUnstampedShippedReport(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ReportName), "unstamped")
	ledger := &byContentLedger{errs: map[string]error{"unstamped": fmt.Errorf(
		"%w: the report names no pg_sage build", earned.ErrBuildMismatch)}}
	res, err := Ingest(context.Background(), ledger, nil,
		Source{Path: dir, Shipped: true, Actor: "release_bench"})
	if err != nil || len(res.Refused) != 1 || len(ledger.refused) != 1 ||
		!ledger.refused[0].RequireBuild {
		t.Fatalf("shipped ingest = %+v (%v), want one refusal", res, err)
	}
}

// Other refusals are not build mismatches: they stay errors on every tick.
func TestIngestKeepsOtherLedgerRefusalsAsErrors(t *testing.T) {
	for name, ledgerErr := range map[string]error{
		"signature mismatch": earned.ErrSignatureMismatch,
		"invalid report":     earned.ErrInvalidReport,
		"store down":         errors.New("connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "r.json"), "r")
			ledger := &byContentLedger{errs: map[string]error{"r": ledgerErr}}
			res, err := Ingest(context.Background(), ledger, nil,
				Source{Path: dir, Actor: "bench_results_path"})
			if !errors.Is(err, ledgerErr) || len(res.Refused) != 0 ||
				!strings.Contains(err.Error(), "r.json") {
				t.Fatalf("ingest = %+v (%v), want the error naming r.json", res, err)
			}
		})
	}
}

// An empty directory refuses nothing.
func TestIngestEmptyDirectoryRefusesNothing(t *testing.T) {
	res, err := Ingest(context.Background(), &byContentLedger{}, nil,
		Source{Path: t.TempDir(), Actor: "bench_results_path"})
	if err != nil || res.Refused != nil || res.Files != 0 {
		t.Fatalf("empty directory = %+v (%v)", res, err)
	}
}
