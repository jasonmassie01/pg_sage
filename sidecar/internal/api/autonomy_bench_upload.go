package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/pg-sage/sidecar/internal/earned"
)

// benches-09: an admin uploads a signed release report (the release's
// pgincidentbench-live.json and its .sigstore.json) as multipart/form-data
// with a "report" and a "bundle" part. The bundle is verified against the
// pg_sage release identity and the report is stored as a signed release
// report that must name the running build. A JSON body stays an unsigned
// operator report.

// BenchVerifier checks a report's Sigstore bundle (benchsig.Verifier).
type BenchVerifier interface {
	Verify(report, bundle []byte) (benchsig.Signature, error)
}

// maxBenchPart bounds a signed upload, both parts together, as the API's
// 1 MiB request limit (maxBodyMiddleware) does.
const maxBenchPart = 1 << 20

// benchParts are a signed upload's report and bundle.
type benchParts struct {
	report, bundle []byte
}

// isMultipartUpload reports whether r is a multipart/form-data upload.
func isMultipartUpload(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "multipart/form-data"
}

// benchUploadSigned verifies and ingests a signed upload; it fails closed
// without verification material.
func (h autonomyHandlers) benchUploadSigned(w http.ResponseWriter, r *http.Request,
	e earned.RegistryEntry) {
	if h.deps.BenchVerifier == nil {
		sreErrorCode(w, "signed bench uploads are unavailable: this pg_sage could not load "+
			"its Sigstore trusted root; install the report with pg_sage bench install",
			"signature_unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBenchPart)
	parts, err := readBenchParts(r)
	if err != nil {
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
		return
	}
	sig, err := h.deps.BenchVerifier.Verify(parts.report, parts.bundle)
	if err != nil {
		sreErrorCode(w, "the bundle is not the pg_sage release signature of this report: "+
			err.Error(), "invalid_signature", http.StatusBadRequest)
		return
	}
	run, err := e.Service.IngestBench(r.Context(), parts.report, earned.BenchIngest{
		Origin: earned.OriginSignedRelease, Actor: autonomyActor(UserFromContext(r.Context())),
		Signature: &earned.ReportSignature{Identity: sig.Identity, Issuer: sig.Issuer,
			Commit: sig.Commit, SignedAt: sig.SignedAt},
		RequireBuild: true})
	if err != nil {
		autonomyError(w, r, err)
		return
	}
	writeBenchRun(w, run)
}

// readBenchParts reads exactly one report and one bundle part, each at
// most maxBenchPart bytes (so is the whole upload).
func readBenchParts(r *http.Request) (benchParts, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return benchParts{}, fmt.Errorf("read the multipart upload: %w", err)
	}
	var out benchParts
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return benchParts{}, uploadReadError(err)
		}
		name := part.FormName()
		if (name != "report" && name != "bundle") || seen[name] {
			return benchParts{}, fmt.Errorf("unexpected part %q: a signed upload has one "+
				"report and one bundle part", name)
		}
		seen[name] = true
		raw, err := io.ReadAll(io.LimitReader(part, maxBenchPart+1))
		if err != nil {
			return benchParts{}, uploadReadError(err)
		}
		if len(raw) > maxBenchPart {
			return benchParts{}, fmt.Errorf("the %s part is larger than 1 MiB; install "+
				"larger reports with pg_sage bench install", name)
		}
		if name == "report" {
			out.report = raw
		} else {
			out.bundle = raw
		}
	}
	switch {
	case len(out.report) == 0:
		return benchParts{}, errors.New("a signed upload needs a non-empty report part")
	case len(out.bundle) == 0:
		return benchParts{}, errors.New("a signed upload needs a non-empty bundle part " +
			"(the report's .sigstore.json)")
	}
	return out, nil
}

// uploadReadError says plainly when the upload hit the size limit.
func uploadReadError(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return errors.New("the upload is larger than 1 MiB; install larger reports with " +
			"pg_sage bench install")
	}
	return fmt.Errorf("read the multipart upload: %w", err)
}
