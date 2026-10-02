package briefing

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// lockedBuffer is a goroutine-safe writer for the concurrency test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type streamWorker struct {
	w      *Worker
	stdout *lockedBuffer
	stderr *lockedBuffer
	mu     sync.Mutex
	logs   []string
}

func newStreamWorker(mcp config.MCPConfig, channels ...string) *streamWorker {
	s := &streamWorker{stdout: &lockedBuffer{}, stderr: &lockedBuffer{}}
	cfg := &config.Config{MCP: mcp, Briefing: config.BriefingConfig{Channels: channels}}
	s.w = &Worker{cfg: cfg, stdout: s.stdout, stderr: s.stderr,
		logFn: func(level, msg string, args ...any) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.logs = append(s.logs, level+": "+fmt.Sprintf(msg, args...))
		}}
	return s
}

func (s *streamWorker) noticeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, line := range s.logs {
		if strings.Contains(line, "MCP stdio") {
			n++
		}
	}
	return n
}

// Phase 0 #9: with the MCP stdio transport, stdout is the JSON-RPC stream.
// The stdout channel is delivered to stderr instead, and the operator is
// told once.
func TestDispatchStdoutGoesToStderrWhenMCPOwnsStdout(t *testing.T) {
	s := newStreamWorker(config.MCPConfig{Enabled: true, Transport: "stdio"}, "stdout")
	s.w.Dispatch(context.Background(), "daily briefing one")
	s.w.Dispatch(context.Background(), "daily briefing two")
	if s.stdout.String() != "" {
		t.Errorf("stdout written while MCP owns it: %q", s.stdout.String())
	}
	got := s.stderr.String()
	if !strings.Contains(got, "daily briefing one\n") ||
		!strings.Contains(got, "daily briefing two\n") {
		t.Errorf("stderr = %q, want both briefings", got)
	}
	if n := s.noticeCount(); n != 1 {
		t.Errorf("redirect notice logged %d times, want once: %v", n, s.logs)
	}
}

func TestDispatchStdoutUsesStdoutWithoutStdioMCP(t *testing.T) {
	for _, mcp := range []config.MCPConfig{
		{Enabled: false, Transport: "stdio"},
		{Enabled: true, Transport: "http"},
		{},
	} {
		s := newStreamWorker(mcp, "stdout")
		s.w.Dispatch(context.Background(), "briefing text")
		if s.stdout.String() != "briefing text\n" {
			t.Errorf("mcp=%+v: stdout = %q, want the briefing", mcp, s.stdout.String())
		}
		if s.stderr.String() != "" || s.noticeCount() != 0 {
			t.Errorf("mcp=%+v: unexpected stderr %q / notices %v", mcp,
				s.stderr.String(), s.logs)
		}
	}
}

// No stdout channel configured: nothing is written and nothing is said.
func TestDispatchWithoutStdoutChannelIsSilent(t *testing.T) {
	s := newStreamWorker(config.MCPConfig{Enabled: true, Transport: "stdio"}, "unknown")
	s.w.Dispatch(context.Background(), "briefing")
	if s.stdout.String() != "" || s.stderr.String() != "" || s.noticeCount() != 0 {
		t.Errorf("stdout=%q stderr=%q logs=%v", s.stdout.String(), s.stderr.String(), s.logs)
	}
}

// Concurrent dispatches (fleet workers share nothing but can overlap with
// a manual trigger) still log the notice once and lose no briefing.
func TestDispatchStdioNoticeOnceUnderConcurrency(t *testing.T) {
	s := newStreamWorker(config.MCPConfig{Enabled: true, Transport: "stdio"}, "stdout")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.w.Dispatch(context.Background(), fmt.Sprintf("briefing-%d", i))
		}(i)
	}
	wg.Wait()
	if n := s.noticeCount(); n != 1 {
		t.Errorf("notice logged %d times, want once", n)
	}
	if s.stdout.String() != "" {
		t.Errorf("stdout written: %q", s.stdout.String())
	}
	if got := strings.Count(s.stderr.String(), "briefing-"); got != 8 {
		t.Errorf("stderr carries %d briefings, want 8", got)
	}
}

// A worker built without explicit streams (as New and older tests do)
// still dispatches without panicking.
func TestDispatchDefaultStreams(t *testing.T) {
	w := &Worker{cfg: &config.Config{
		MCP:      config.MCPConfig{Enabled: true, Transport: "stdio"},
		Briefing: config.BriefingConfig{Channels: []string{"stdout"}},
	}, logFn: func(string, string, ...any) {}}
	w.Dispatch(context.Background(), "x")
	out, errOut := w.streams()
	if out == nil || errOut == nil {
		t.Fatal("default streams are nil")
	}
}
