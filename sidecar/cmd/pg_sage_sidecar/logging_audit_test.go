package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// No parallel tests: this checks the process stderr logging boundary.
func TestAuditLogWrapperPreservesSeverity(t *testing.T) {
	previous := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	os.Stderr = writer
	defer func() { os.Stderr = previous }()
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
