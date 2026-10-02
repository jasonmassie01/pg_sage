package runway

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// scriptedSizeRunner answers cluster_database_size from a script: one
// result per call, the last one repeated.
type scriptedSizeRunner struct {
	script []probes.Result
	calls  int
}

func (s *scriptedSizeRunner) Run(
	_ context.Context, id probes.ID, _ probes.Args,
) probes.Result {
	if id != probes.ClusterDatabaseSizeProbe {
		return probes.Result{ProbeID: id, Status: probes.StatusEmpty}
	}
	i := s.calls
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	s.calls++
	return s.script[i]
}

func (s *scriptedSizeRunner) RunBackground(
	ctx context.Context, id probes.ID, a probes.Args,
) probes.Result {
	return s.Run(ctx, id, a)
}

func sizeOK(bytes int64) probes.Result {
	return probes.Result{ProbeID: probes.ClusterDatabaseSizeProbe, Status: probes.StatusOK,
		Rows: []probes.Row{{"database_bytes": bytes, "databases_unreadable": int64(0)}}}
}

func sizeFailed() probes.Result {
	return probes.Result{ProbeID: probes.ClusterDatabaseSizeProbe,
		Status: probes.StatusError, Reason: "query_failed",
		Error: "database with OID 16384 does not exist"}
}

// A database dropped while it is being sized fails the whole query; the
// measurement is repeated once instead of losing the pass.
func TestMeasureSize_RetriesOnceAfterAFailedQuery(t *testing.T) {
	r := &scriptedSizeRunner{script: []probes.Result{sizeFailed(), sizeOK(4096)}}
	size, err := measureSize(context.Background(), r)
	if err != nil {
		t.Fatalf("measureSize: %v", err)
	}
	if size.DatabaseBytes != 4096 || size.UnreadableDatabases != 0 || r.calls != 2 {
		t.Fatalf("size = %+v after %d calls, want 4096 bytes after 2", size, r.calls)
	}
}

func TestMeasureSize_SecondFailureIsReported(t *testing.T) {
	r := &scriptedSizeRunner{script: []probes.Result{sizeFailed(), sizeFailed()}}
	if _, err := measureSize(context.Background(), r); err == nil {
		t.Fatal("two failed measurements returned no error")
	}
	if r.calls != 2 {
		t.Fatalf("calls = %d, want exactly one retry", r.calls)
	}
}

func TestMeasureSize_SuccessIsNotRepeated(t *testing.T) {
	r := &scriptedSizeRunner{script: []probes.Result{sizeOK(8192)}}
	size, err := measureSize(context.Background(), r)
	if err != nil || size.DatabaseBytes != 8192 || r.calls != 1 {
		t.Fatalf("size = %+v, err = %v, calls = %d; want 8192 in one call",
			size, err, r.calls)
	}
}

// A cancelled pass is not retried.
func TestMeasureSize_CancelledContextIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &scriptedSizeRunner{script: []probes.Result{sizeFailed(), sizeOK(1)}}
	if _, err := measureSize(ctx, r); err == nil {
		t.Fatal("cancelled measurement returned no error")
	}
	if r.calls != 1 {
		t.Fatalf("calls = %d, want no retry after cancellation", r.calls)
	}
}
