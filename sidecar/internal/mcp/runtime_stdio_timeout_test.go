package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Per-call deadline, stdout write failures and the bounded line reader of
// the stdio transport (mcp-04).

// Error propagation: a call past the call timeout is answered -32800 and
// the backend sees DeadlineExceeded.
func TestStdioCallPastTimeoutReturnsTimedOut(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdioWith(t, NewServer(backend), func(r *Runtime) {
		r.SetCallTimeout(100 * time.Millisecond)
	})
	peer.send(callLine("slow"))
	backend.waitStarted(t)
	require.ErrorIs(t, backend.waitEnded(t), context.DeadlineExceeded)
	answered := peer.next()
	require.Equal(t, "slow", answered["id"])
	code, text := rpcErrorOf(t, answered)
	require.Equal(t, float64(codeCancelled), code)
	require.Contains(t, text, "timed out after 100ms")
	require.Contains(t, text, "mcp.call_timeout_seconds")
	peer.requireServing()
}

// The runtime takes its call timeout from mcp.call_timeout_seconds, and a
// zero (unset in code) value keeps the 90 s default.
func TestStdioCallTimeoutComesFromConfig(t *testing.T) {
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio",
		CallTimeoutSeconds: 12}, NewServer(&recordingBackend{}), strings.NewReader(""),
		&bytes.Buffer{})
	require.NoError(t, err)
	require.Equal(t, 12*time.Second, runtime.callTimeout)
	runtime, err = NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(&recordingBackend{}), strings.NewReader(""), &bytes.Buffer{})
	require.NoError(t, err)
	require.Equal(t, 90*time.Second, runtime.callTimeout)
	runtime.SetCallTimeout(0)
	require.Equal(t, 90*time.Second, runtime.callTimeout)
}

var errStdoutGone = errors.New("stdout gone")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errStdoutGone }

// Error propagation: a failed stdout write ends Serve with that error,
// whether an inline answer or a tool call's answer hit it.
func TestStdioWriteErrorEndsServe(t *testing.T) {
	for name, line := range map[string]string{"inline": pingLine(1),
		"tool call": callLine("c1")} {
		t.Run(name, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio"},
				NewServer(&recordingBackend{}), reader, failingWriter{})
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { done <- runtime.Serve(context.Background()) }()
			_, err = writer.Write([]byte(line + "\n"))
			require.NoError(t, err)
			select {
			case err := <-done:
				require.ErrorIs(t, err, errStdoutGone)
			case <-time.After(5 * time.Second):
				t.Fatal("Serve kept running after stdout failed")
			}
		})
	}
}

var errStdinBroken = errors.New("stdin broken")

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errStdinBroken }

// Error propagation: a read error other than EOF is fatal and returned.
func TestStdioReadErrorEndsServe(t *testing.T) {
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(&recordingBackend{}), brokenReader{}, &bytes.Buffer{})
	require.NoError(t, err)
	err = runtime.Serve(context.Background())
	require.ErrorIs(t, err, errStdinBroken)
	require.ErrorContains(t, err, "stdin")
}

// State transition: a fatal stdin error with a call in flight cancels that
// call (the backend sees context.Canceled) and Serve returns the read error
// at once instead of waiting out the call timeout. EOF, by contrast, lets
// the call finish (TestStdioEOFWaitsForInFlightCall).
func TestStdioReadErrorCancelsInFlightCall(t *testing.T) {
	backend := newBlockingBackend()
	peer := startStdioWith(t, NewServer(backend), func(r *Runtime) {
		r.SetCallTimeout(time.Minute)
	})
	peer.send(callLine("c1"))
	backend.waitStarted(t)
	require.NoError(t, peer.in.CloseWithError(errStdinBroken))
	require.ErrorIs(t, backend.waitEnded(t), context.Canceled)
	requireServeEnds(t, peer.done, errStdinBroken)
}

// State transition: a failed stdout write (here an inline ping answer)
// with a call in flight cancels that call and ends Serve with the write
// error at once.
func TestStdioWriteErrorCancelsInFlightCall(t *testing.T) {
	backend := newBlockingBackend()
	reader, writer := io.Pipe()
	defer writer.Close()
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(backend), reader, failingWriter{})
	require.NoError(t, err)
	runtime.SetCallTimeout(time.Minute)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background()) }()
	_, err = writer.Write([]byte(callLine("c1") + "\n"))
	require.NoError(t, err)
	backend.waitStarted(t)
	_, err = writer.Write([]byte(pingLine(2) + "\n"))
	require.NoError(t, err)
	require.ErrorIs(t, backend.waitEnded(t), context.Canceled)
	requireServeEnds(t, done, errStdoutGone)
}

// Error propagation: at EOF Serve waits for the in-flight call, and when
// writing that call's answer fails it returns the write error, not nil.
func TestStdioWriteErrorAfterEOFIsReturned(t *testing.T) {
	backend := newBlockingBackend()
	reader, writer := io.Pipe()
	runtime, err := NewRuntime(config.MCPConfig{Enabled: true, Transport: "stdio"},
		NewServer(backend), reader, failingWriter{})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background()) }()
	_, err = writer.Write([]byte(callLine("c1") + "\n"))
	require.NoError(t, err)
	backend.waitStarted(t)
	require.NoError(t, writer.Close())
	select {
	case err := <-done:
		t.Fatalf("Serve returned %v before the in-flight call finished", err)
	case <-time.After(50 * time.Millisecond):
	}
	backend.release <- struct{}{}
	require.NoError(t, backend.waitEnded(t))
	requireServeEnds(t, done, errStdoutGone)
}

func requireServeEnds(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		require.ErrorIs(t, err, want)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve waited for the in-flight call after a fatal I/O error")
	}
}

func readAllBounded(t *testing.T, input string, limit int) ([]string, []bool) {
	t.Helper()
	reader := bufio.NewReaderSize(strings.NewReader(input), 16)
	var lines []string
	var oversized []bool
	for {
		line, over, err := readBoundedLine(reader, limit)
		if errors.Is(err, io.EOF) {
			return lines, oversized
		}
		require.NoError(t, err)
		lines = append(lines, string(line))
		oversized = append(oversized, over)
	}
}

// Boundary: with a reader buffer smaller than the limit, a line of exactly
// the limit is whole, limit+1 is oversized and discarded up to its
// newline, and a final line without a newline is still returned.
func TestStdioReadBoundedLine(t *testing.T) {
	exact := strings.Repeat("a", 40)
	over := strings.Repeat("b", 41)
	lines, oversized := readAllBounded(t,
		exact+"\n"+over+"\n"+"next\r\n"+"\n"+over+over+"\n"+"tail", 40)
	require.Equal(t, []string{exact, "", "next", "", "", "tail"}, lines)
	require.Equal(t, []bool{false, true, false, false, true, false}, oversized)
}

// Nil/empty: empty input is EOF at once; an oversized last line with no
// newline is still reported as oversized.
func TestStdioReadBoundedLineEdges(t *testing.T) {
	lines, _ := readAllBounded(t, "", 8)
	require.Empty(t, lines)
	lines, oversized := readAllBounded(t, strings.Repeat("z", 9), 8)
	require.Equal(t, []string{""}, lines)
	require.Equal(t, []bool{true}, oversized)
	lines, oversized = readAllBounded(t, strings.Repeat("z", 8)+"\r\n", 8)
	require.Equal(t, []string{strings.Repeat("z", 8)}, lines)
	require.Equal(t, []bool{false}, oversized)
}
