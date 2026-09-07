package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRestartHandler_NotSupported(t *testing.T) {
	restartFunc = nil // no supervisor wired
	w := httptest.NewRecorder()
	restartHandler(w, httptest.NewRequest("POST", "/api/v1/restart", nil))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when no restart func", w.Code)
	}
}

func TestRestartHandler_TriggersRestart(t *testing.T) {
	var called atomic.Bool
	SetRestartFunc(func() { called.Store(true) })
	defer SetRestartFunc(nil)

	w := httptest.NewRecorder()
	restartHandler(w, httptest.NewRequest("POST", "/api/v1/restart", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The restart fires on a short delay after responding.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if called.Load() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("restart func was not invoked")
}
