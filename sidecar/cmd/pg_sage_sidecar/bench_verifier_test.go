package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/benchingest"
	"github.com/pg-sage/sidecar/internal/earned"
)

// benches-11: every database's ingester asks for the release verifier on
// every tick, and building one parses the embedded Sigstore trusted root,
// so the process builds it once and keeps it; a failed build is retried.

// countingBuilds makes newBenchVerifier count its builds; the builds whose
// 1-based number is in fail return an error.
func countingBuilds(t *testing.T, fail ...int32) *atomic.Int32 {
	t.Helper()
	builds := &atomic.Int32{}
	failing := map[int32]bool{}
	for _, n := range fail {
		failing[n] = true
	}
	old := newBenchVerifier
	newBenchVerifier = func() (benchingest.Verifier, error) {
		if failing[builds.Add(1)] {
			return nil, errors.New("embedded trusted root unreadable")
		}
		return releaseFakeVerifier{commit: releaseCommit}, nil
	}
	benchVerifiers.reset()
	t.Cleanup(func() {
		newBenchVerifier = old
		benchVerifiers.reset()
	})
	return builds
}

func TestBenchVerifierIsBuiltOnceAcrossIngestTicks(t *testing.T) {
	builds := countingBuilds(t)
	for tick := 1; tick <= 3; tick++ {
		res, err := ingestBenchSources(context.Background(), nil, nil)
		if err != nil || res.Files != 0 {
			t.Fatalf("tick %d: ingest of no sources = %+v, %v", tick, res, err)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("3 ingest ticks built the verifier %d times, want 1", got)
	}
}

func TestBenchVerifierFailedBuildIsRetriedThenKept(t *testing.T) {
	builds := countingBuilds(t, 1)
	if v, err := benchVerifier(); err == nil || v != nil ||
		!strings.Contains(err.Error(), "trusted root unreadable") {
		t.Fatalf("first ask = %v, %v; want the build's error", v, err)
	}
	for ask := 2; ask <= 4; ask++ {
		v, err := benchVerifier()
		if err != nil || v != (releaseFakeVerifier{commit: releaseCommit}) {
			t.Fatalf("ask %d = %v, %v; want the rebuilt verifier", ask, v, err)
		}
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("a failed build then 3 asks built %d times, want 2 (one retry)", got)
	}
}

func TestBenchVerifierNilWithoutErrorIsAnErrorNotCached(t *testing.T) {
	builds := &atomic.Int32{}
	old := newBenchVerifier
	newBenchVerifier = func() (benchingest.Verifier, error) {
		builds.Add(1)
		return nil, nil
	}
	benchVerifiers.reset()
	t.Cleanup(func() {
		newBenchVerifier = old
		benchVerifiers.reset()
	})
	for ask := 1; ask <= 2; ask++ {
		v, err := benchVerifier()
		if err == nil || v != nil || !strings.Contains(err.Error(), "verifier is missing") {
			t.Fatalf("ask %d = %v, %v; want a missing-verifier error", ask, v, err)
		}
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("nil verifier builds = %d, want 2 (never cached)", got)
	}
	if _, err := verifyReportFile(filepath.Join(t.TempDir(), "r.json"), ""); err == nil ||
		!strings.Contains(err.Error(), "trusted root") {
		t.Fatalf("verify with no verifier = %v, want the trusted-root error", err)
	}
}

func TestBenchVerifierIsSharedByIngestAndVerify(t *testing.T) {
	builds := countingBuilds(t)
	path := writeShippedReport(t, t.TempDir(), "core", time.Now().UTC().Add(-time.Hour),
		earned.Build{Version: "1.8.5", Commit: releaseCommit}, "lock_blocking", true)
	if _, err := ingestBenchSources(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		sig, err := verifyReportFile(path, releaseCommit)
		if err != nil || sig.Commit != releaseCommit {
			t.Fatalf("verify %d = %+v, %v", i, sig, err)
		}
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("one ingest and two verifies built %d times, want 1", got)
	}
}

// -race is unavailable on this machine (no cgo); CI runs this under it.
func TestBenchVerifierParallelIngestersShareOneBuild(t *testing.T) {
	builds := countingBuilds(t)
	const ingesters = 32
	got := make([]benchingest.Verifier, ingesters)
	errs := make([]error, ingesters)
	var wg sync.WaitGroup
	for i := 0; i < ingesters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = benchVerifier()
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != (releaseFakeVerifier{commit: releaseCommit}) {
			t.Fatalf("ingester %d = %v, %v", i, got[i], errs[i])
		}
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d parallel ingesters built the verifier %d times, want 1", ingesters, n)
	}
}

func TestWithVerifierResetsTheCache(t *testing.T) {
	countingBuilds(t)
	if _, err := benchVerifier(); err != nil {
		t.Fatal(err)
	}
	withVerifier(t, nil, errors.New("no trusted root"))
	if v, err := benchVerifier(); err == nil || v != nil {
		t.Fatalf("after withVerifier a cached verifier was served: %v, %v", v, err)
	}
}
