package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// logOutput, when set, receives the log lines instead of os.Stderr. Each
// line reads both under logOutputMu, and tests change them only under it
// (setLogOutput, and swapStderr for os.Stderr): runtime workers a test
// started, or that earlier tests left draining, still log meanwhile.
var (
	logOutputMu sync.RWMutex
	logOutput   io.Writer
)

// setLogOutput sends the log lines to w (nil: os.Stderr) and returns the
// previous writer.
func setLogOutput(w io.Writer) io.Writer {
	logOutputMu.Lock()
	defer logOutputMu.Unlock()
	old := logOutput
	logOutput = w
	return old
}

func logInfo(component, msg string, args ...any) { logStructured("INFO", component, msg, args...) }
func logWarn(component, msg string, args ...any) { logStructured("WARN", component, msg, args...) }
func logError(component, msg string, args ...any) {
	logStructured("ERROR", component, msg, args...)
}

func logStructured(level, component, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339)
	logOutputMu.RLock()
	out := logOutput
	if out == nil {
		out = os.Stderr
	}
	logOutputMu.RUnlock()
	_, _ = fmt.Fprintf(out, "%s [%s] [%s] %s\n", ts, level, component, fmt.Sprintf(msg, args...))
}

func logStructuredWrapper(component, msg string, args ...any) {
	switch level := strings.ToUpper(component); level {
	case "DEBUG", "INFO", "WARN", "ERROR":
		logStructured(level, "sidecar", msg, args...)
		return
	}
	logStructured("INFO", component, msg, args...)
}
