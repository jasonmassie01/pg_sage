package main

import (
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// No parallel tests: this checks the process stderr logging boundary.
func TestAuditLogWrapperPreservesSeverity(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	previous := swapStderr(writer)
	defer swapStderr(previous)
	logStructuredWrapper("WARN", "database unavailable: %s", "offline")
	logStructuredWrapper("executor", "cycle %d", 2)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	text := string(output)
	if !strings.Contains(text, "[WARN] [sidecar] database unavailable: offline") ||
		!strings.Contains(text, "[INFO] [executor] cycle 2") {
		t.Fatalf("severity/component contract lost: %s", text)
	}
}

// Concurrent access: a runtime's workers log while its test captures
// stderr (the reconnected member's review grade during the second capture
// of TestRetryFailedInstancesIsolatesUnreachableMember). Under -race this
// fails when logStructured reads os.Stderr outside the lock swapStderr
// takes, or when a capture swaps os.Stderr without it.
func TestCaptureStderrWhileWorkersLog(t *testing.T) {
	const line = "[INFO] [worker] line from worker"
	original := os.Stderr
	captured := 0
	// The outer capture takes the lines logged between the inner ones.
	captureStderr(t, func() {
		outer := os.Stderr
		stop := startLoggingWorkers(4)
		defer stop()
		deadline := time.Now().Add(10 * time.Second)
		for i := 0; i < 20 || (captured == 0 && time.Now().Before(deadline)); i++ {
			out := captureStderr(t, func() { time.Sleep(2 * time.Millisecond) })
			captured += strings.Count(out, line)
			if os.Stderr != outer {
				t.Fatalf("capture %d left os.Stderr at %v, want the outer pipe", i, os.Stderr)
			}
		}
	})
	if os.Stderr != original {
		t.Fatalf("os.Stderr = %v after the captures, want %v", os.Stderr, original)
	}
	if captured == 0 {
		t.Fatal("no worker line landed in a capture while the workers logged")
	}
}

// startLoggingWorkers starts n goroutines that log until stop, which waits
// for them.
func startLoggingWorkers(n int) (stop func()) {
	done := make(chan struct{})
	var workers sync.WaitGroup
	for w := range n {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				logInfo("worker", "line from worker %d", w)
				time.Sleep(100 * time.Microsecond)
			}
		}()
	}
	return func() {
		close(done)
		workers.Wait()
	}
}
