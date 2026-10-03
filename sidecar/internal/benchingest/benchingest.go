// Package benchingest feeds PGIncidentBench reports from disk to the
// earned-autonomy ledger (roadmap 1.1). Shipped reports (in the image or
// next to the binary) must name the running build; the operator's
// sre.autonomy.bench_results_path need not. A report with a Sigstore
// bundle next to it ("<report>.sigstore.json") is verified: a valid
// signature makes it a signed release report, an invalid one refuses it;
// without a bundle (or without verification material) it is an unsigned
// operator-provided report.
package benchingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/earned"
)

const (
	// ImageDir is where the container image carries its release reports.
	ImageDir = "/usr/share/pg_sage/bench"
	// ReportName is the file name of a bench report (one per CI shard).
	ReportName = "pgincidentbench.json"
	// BundleSuffix names a report's Sigstore bundle: <report><suffix>.
	BundleSuffix = ".sigstore.json"
	// MaxDepth bounds the walk of a report directory: CI writes one
	// report per shard one level down ({core,reactive,runway}).
	MaxDepth = 3
	// maxBundleBytes bounds a signature bundle.
	maxBundleBytes = 1 << 20
)

// Ledger stores reports (earned.Service).
type Ledger interface {
	IngestBench(ctx context.Context, raw []byte, in earned.BenchIngest) (earned.EvalRun,
		error)
}

// Verifier checks a report's signature bundle (benchsig.Verifier).
type Verifier interface {
	Verify(report, bundle []byte) (benchsig.Signature, error)
}

// Source is a report file or directory.
type Source struct {
	Path string
	// Shipped: the reports came with this pg_sage (image or release
	// archive). Each must name the running build; a missing directory is
	// not an error.
	Shipped bool
	// Actor is recorded as the ingesting identity.
	Actor string
}

// Result counts one ingest: report files read, new reports stored and
// verified signatures.
type Result struct {
	Files, Added, Signed int
}

// ShippedDirs are the directories shipped reports may be in: the image's,
// and bench/ next to the executable (the release archive's layout).
func ShippedDirs(executable string) []string {
	dirs := []string{ImageDir}
	if executable != "" {
		dirs = append(dirs, filepath.Join(filepath.Dir(executable), "bench"))
	}
	return dirs
}

// Ingest reads every report of src into the ledger. One report's failure
// does not stop the others; the errors are joined, each naming its file.
func Ingest(ctx context.Context, ledger Ledger, verifier Verifier, src Source) (Result,
	error) {
	if ledger == nil || strings.TrimSpace(src.Path) == "" {
		return Result{}, errors.New("bench ingest needs a ledger and a path")
	}
	files, err := sourceFiles(src)
	if err != nil || len(files) == 0 {
		return Result{}, err
	}
	var res Result
	var errs []error
	for _, f := range files {
		res.Files++
		run, signed, err := ingestFile(ctx, ledger, verifier, src, f)
		if err != nil {
			errs = append(errs, fmt.Errorf("bench report %s: %w", f, err))
			continue
		}
		if signed {
			res.Signed++
		}
		if !run.Duplicate {
			res.Added++
		}
	}
	return res, errors.Join(errs...)
}

// sourceFiles lists src's reports; a shipped directory may be absent.
func sourceFiles(src Source) ([]string, error) {
	info, err := os.Stat(src.Path)
	switch {
	case src.Shipped && errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("bench results path: %w", err)
	case !info.IsDir():
		return []string{src.Path}, nil
	}
	files, err := ReportFiles(src.Path)
	if err != nil {
		return nil, fmt.Errorf("bench results path: %w", err)
	}
	return files, nil
}

func ingestFile(ctx context.Context, ledger Ledger, verifier Verifier, src Source,
	path string) (earned.EvalRun, bool, error) {
	raw, err := readBounded(path, earned.MaxReportBytes)
	if err != nil {
		return earned.EvalRun{}, false, err
	}
	in := earned.BenchIngest{Origin: earned.OriginOperator, Actor: src.Actor,
		RequireBuild: src.Shipped}
	sig, err := signatureOf(verifier, path, raw)
	if err != nil {
		return earned.EvalRun{}, false, err
	}
	if sig != nil {
		in.Origin, in.Signature = earned.OriginSignedRelease, sig
	}
	run, err := ledger.IngestBench(ctx, raw, in)
	return run, sig != nil, err
}

// signatureOf verifies the report's bundle: nil without a bundle or
// without a verifier, an error when the bundle does not verify.
func signatureOf(verifier Verifier, path string, raw []byte) (*earned.ReportSignature,
	error) {
	if verifier == nil {
		return nil, nil
	}
	bundle, err := readBounded(path+BundleSuffix, maxBundleBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sig, err := verifier.Verify(raw, bundle)
	if err != nil {
		return nil, fmt.Errorf("signature %s: %w", filepath.Base(path+BundleSuffix), err)
	}
	return &earned.ReportSignature{Identity: sig.Identity, Issuer: sig.Issuer,
		Commit: sig.Commit, SignedAt: sig.SignedAt}, nil
}

// readBounded reads path, refusing files larger than limit.
func readBounded(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), limit)
	}
	return raw, nil
}

// ReportFiles lists the report (*.json) files under root, at most
// MaxDepth levels down, sorted; signature bundles are not reports.
func ReportFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		depth := len(strings.Split(filepath.ToSlash(rel), "/"))
		if d.IsDir() {
			if rel != "." && depth > MaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".json") && !strings.HasSuffix(d.Name(), BundleSuffix) {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}
