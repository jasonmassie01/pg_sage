package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func logInfo(component, msg string, args ...any) { logStructured("INFO", component, msg, args...) }
func logWarn(component, msg string, args ...any) { logStructured("WARN", component, msg, args...) }
func logError(component, msg string, args ...any) {
	logStructured("ERROR", component, msg, args...)
}

func logStructured(level, component, msg string, args ...any) {
	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(os.Stderr, "%s [%s] [%s] %s\n", ts, level, component, fmt.Sprintf(msg, args...))
}

func logStructuredWrapper(component, msg string, args ...any) {
	switch level := strings.ToUpper(component); level {
	case "DEBUG", "INFO", "WARN", "ERROR":
		logStructured(level, "sidecar", msg, args...)
		return
	}
	logStructured("INFO", component, msg, args...)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
