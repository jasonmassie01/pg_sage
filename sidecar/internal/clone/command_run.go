package clone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/sre"
)

// commandWaitDelay is how long a killed program's pipes may stay open
// (a child it started) before Wait gives up on them.
const commandWaitDelay = 5 * time.Second

// run runs argv with idName=id in its environment, bounded by limit, and
// returns its stdout. A non-zero exit is ErrCommandFailed quoting the
// redacted stderr tail; a timeout wraps the context's error.
// Output held open after a successful exit is ErrCommandOutputHeld, an
// uncertain create.
func (p *CommandProvider) run(ctx context.Context, argv []string, limit time.Duration,
	bound, idName, id string) ([]byte, error) {
	if err := os.MkdirAll(p.cfg.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("restore command: create work directory %s: %w",
			p.cfg.WorkDir, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = p.cfg.WorkDir, p.env(idName, id), commandWaitDelay
	stdout := &headBuffer{limit: CommandMaxStdout}
	stderr := &tailBuffer{limit: CommandMaxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if ctxErr := runCtx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("restore command %s did not finish within %s (%s), "+
			"killed: %w%s", argv[0], limit, bound, ctxErr, stderrQuote(stderr))
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// It exited 0, but what it started (a server) still holds its output.
		return nil, fmt.Errorf("%w: %s exited 0 but a process it started held its stdout "+
			"or stderr open for %s more; start servers with pg_ctl -l or redirect their "+
			"output: %w", ErrCommandOutputHeld, argv[0], commandWaitDelay, ErrCloneLeftBehind)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil, fmt.Errorf("%w: %s exited with status %d%s", ErrCommandFailed, argv[0],
			exitErr.ExitCode(), stderrQuote(stderr))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: start %s: %w", ErrCommandFailed, argv[0], err)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("%w: %s printed more than %d bytes: %w", ErrCommandOutputTooLarge,
			argv[0], CommandMaxStdout, ErrCloneLeftBehind)
	}
	return stdout.buf.Bytes(), nil
}

// commandBaseEnv are the sidecar variables every program gets.
var commandBaseEnv = []string{"PATH"}

// env is the program's whole environment: the base variables, the
// allowed ones that are set, the drilled database (restore only), then
// idName=id.
func (p *CommandProvider) env(idName, id string) []string {
	names := append([]string(nil), commandBaseEnv...)
	if runtime.GOOS == "windows" {
		names = append(names, "SystemRoot")
	}
	names = append(names, p.cfg.EnvAllow...)
	out := make([]string, 0, len(names)+2)
	for _, name := range names {
		if v, ok := p.cfg.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	if idName == "SAGE_DRILL_ID" && p.cfg.Database != "" {
		out = append(out, "SAGE_DRILL_DATABASE="+p.cfg.Database)
	}
	return append(out, idName+"="+id)
}

// stderrQuote is the redacted stderr tail as an error suffix.
func stderrQuote(stderr *tailBuffer) string {
	tail := strings.TrimSpace(sre.RedactText(stderr.String()))
	if tail == "" {
		return " (no stderr)"
	}
	return fmt.Sprintf("; stderr tail: %q", tail)
}

// commandLine is the restore program's one line of output.
type commandLine struct {
	DSN           string `json:"dsn"`
	ID            string `json:"id"`
	RecoveryPoint string `json:"recovery_point"`
}

// parseCommandLine reads stdout: one JSON object on one line naming a
// database other than source and a recovery point no later than now.
func parseCommandLine(out []byte, source string, now time.Time) (Clone, error) {
	text := strings.TrimSpace(string(out))
	var line commandLine
	dec := json.NewDecoder(strings.NewReader(text))
	if text == "" || strings.ContainsAny(text, "\r\n") || dec.Decode(&line) != nil ||
		dec.More() {
		return Clone{}, fmt.Errorf("%w: got %q", ErrCommandBadOutput, snippet(text))
	}
	if strings.TrimSpace(line.DSN) == "" {
		return Clone{}, fmt.Errorf("%w: dsn", ErrCommandMissingField)
	}
	if err := validCommandCloneID(line.ID); err != nil {
		return Clone{}, err
	}
	at, err := time.Parse(time.RFC3339, line.RecoveryPoint)
	if err != nil {
		return Clone{}, fmt.Errorf("%w: recovery_point must be RFC3339, got %q",
			ErrCommandMissingField, snippet(line.RecoveryPoint))
	}
	if at.After(now) {
		return Clone{}, fmt.Errorf("%w: recovery_point %s is in the future",
			ErrCommandBadOutput, at.Format(time.RFC3339))
	}
	if err := refuseSourceDSN(line.DSN, source); err != nil {
		return Clone{}, err
	}
	return Clone{DSN: line.DSN, ID: line.ID, CreatedFrom: at.UTC()}, nil
}

// refuseSourceDSN refuses a dsn that names the source's host, port and
// database (the identity guard checks the server again on connect).
func refuseSourceDSN(dsn, source string) error {
	got, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("%w: dsn does not parse: %s", ErrCommandBadOutput,
			sre.RedactText(err.Error()))
	}
	if source == "" {
		return nil
	}
	want, err := pgconn.ParseConfig(source)
	if err != nil {
		return fmt.Errorf("restore command: source dsn does not parse: %s",
			sre.RedactText(err.Error()))
	}
	if strings.EqualFold(got.Host, want.Host) && got.Port == want.Port &&
		got.Database == want.Database {
		return fmt.Errorf("%w: %s:%d/%s", ErrCommandDSNIsSource, got.Host, got.Port,
			got.Database)
	}
	return nil
}

// snippet is the redacted start of untrusted output for an error.
func snippet(s string) string {
	const limit = 200
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return sre.RedactText(s)
}

// headBuffer keeps the first limit bytes and notes any more.
type headBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (b *headBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if len(p) > room {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// tailBuffer keeps the last limit bytes.
type tailBuffer struct {
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
