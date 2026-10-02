package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Characterization of main()'s wiring before it is split into helpers: the
// test binary re-runs itself as the sidecar process (TestMainProcessChild)
// and the parent asserts which components start, what they serve, and how
// the process stops, for each mode.

const mainChildEnv = "PG_SAGE_MAIN_CHAR_ARGS"

// mainChildDSNEnvs are the variables testdb points at a disabled server in
// the child; the sidecar must only see the config the parent wrote.
var mainChildDSNEnvs = []string{
	"SAGE_TEST_DATABASE_URL", "SAGE_DATABASE_URL", "SAGE_TEST_DSN", "PG_TEST_DSN",
	"PIPELINE_PG_URL", "HINT_TEST_DSN",
}

// TestMainProcessChild is the child entry point; it does nothing in a normal
// test run.
func TestMainProcessChild(t *testing.T) {
	args := os.Getenv(mainChildEnv)
	if args == "" {
		return
	}
	for _, name := range mainChildDSNEnvs[1:] {
		_ = os.Unsetenv(name)
	}
	os.Args = append([]string{"pg_sage"}, strings.Split(args, "\x1f")...)
	main()
}

// syncBuffer collects the child's output while the parent polls it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type mainChild struct {
	cmd  *osexec.Cmd
	out  *syncBuffer
	done chan error
}

func startMainChild(t *testing.T, env []string, args ...string) *mainChild {
	t.Helper()
	cmd := osexec.Command(os.Args[0], "-test.run=^TestMainProcessChild$")
	cmd.Env = append(childBaseEnv(), mainChildEnv+"="+strings.Join(args, "\x1f"))
	cmd.Env = append(cmd.Env, env...)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sidecar child: %v", err)
	}
	child := &mainChild{cmd: cmd, out: out, done: make(chan error, 1)}
	go func() { child.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			<-child.done
		}
	})
	return child
}

func childBaseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name := strings.SplitN(kv, "=", 2)[0]
		if !isMainChildDSNEnv(name) && name != "SAGE_SUPERVISED" &&
			!strings.HasPrefix(name, "SAGE_LLM") && name != "GEMINI_API_KEY" {
			env = append(env, kv)
		}
	}
	return env
}

func isMainChildDSNEnv(name string) bool {
	for _, n := range mainChildDSNEnvs {
		if n == name {
			return true
		}
	}
	return false
}

// wait returns the exit code once the child stops, failing after timeout.
func (c *mainChild) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case err := <-c.done:
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		if err != nil {
			t.Fatalf("wait for sidecar child: %v", err)
		}
		return 0
	case <-time.After(timeout):
		t.Fatalf("sidecar child still running after %s; output:\n%s", timeout,
			redactAdminPassword(c.out.String()))
		return -1
	}
}

func (c *mainChild) waitForOutput(t *testing.T, timeout time.Duration, want ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if containsAll(c.out.String(), want) {
			return
		}
		select {
		case <-c.done:
			t.Fatalf("sidecar child exited before %q; output:\n%s", want,
				redactAdminPassword(c.out.String()))
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("sidecar child did not log %q within %s; output:\n%s", want, timeout,
		redactAdminPassword(c.out.String()))
}

func (c *mainChild) terminate(t *testing.T) int {
	t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal sidecar child: %v", err)
	}
	return c.wait(t, 30*time.Second)
}

// waitForListeners dials each addr until it accepts: the sidecar logs
// "listening on" from the server goroutine just before it binds, so the log
// line alone does not mean the port is open.
func waitForListeners(t *testing.T, timeout time.Duration, addrs ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for _, addr := range addrs {
		for {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				_ = conn.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never accepted connections: %v", addr, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func containsAll(s string, want []string) bool {
	for _, w := range want {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}

var adminPasswordLine = regexp.MustCompile(`INITIAL ADMIN PASSWORD: \S+`)

func redactAdminPassword(out string) string {
	return adminPasswordLine.ReplaceAllString(out, "INITIAL ADMIN PASSWORD: [redacted]")
}

func adminPasswordFrom(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`INITIAL ADMIN PASSWORD: (\S+) \*\*\*`).FindStringSubmatch(out)
	if m == nil {
		t.Fatal("child did not bootstrap an admin user")
	}
	return m[1]
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func writeChildConfig(t *testing.T, body string) string {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write child config: %v", err)
	}
	return path
}

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func listenAddrs(t *testing.T) (api, prom string) {
	t.Helper()
	return fmt.Sprintf("127.0.0.1:%d", freePort(t)), fmt.Sprintf("127.0.0.1:%d", freePort(t))
}
