package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// logOutput, when set, receives the log lines instead of os.Stderr (read
// at each line, so tests that swap os.Stderr keep working); tests set it.
var logOutput io.Writer

func logInfo(component, msg string, args ...any) { logStructured("INFO", component, msg, args...) }
func logWarn(component, msg string, args ...any) { logStructured("WARN", component, msg, args...) }
func logError(component, msg string, args ...any) {
	logStructured("ERROR", component, msg, args...)
}

func logStructured(level, component, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339)
	out := logOutput
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "%s [%s] [%s] %s\n", ts, level, component, fmt.Sprintf(msg, args...))
}

func logStructuredWrapper(component, msg string, args ...any) {
	switch level := strings.ToUpper(component); level {
	case "DEBUG", "INFO", "WARN", "ERROR":
		logStructured(level, "sidecar", msg, args...)
		return
	}
	logStructured("INFO", component, msg, args...)
}
