package logwatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// startupLookbackBytes is how far from EOF we seek on first open.
// This lets us pick up recent log context without reading the entire file.
const startupLookbackBytes int64 = 1024 * 1024

// Tailer follows a single PostgreSQL log file and emits new lines.
// It uses polling (no fsnotify) and handles copytruncate rotation
// as well as new-file rotation.
type Tailer struct {
	dir          string
	format       string // "jsonlog" or "csvlog"
	pollInterval time.Duration
	maxLineLen   int
	logFn        func(string, string, ...any)

	// internal state — guarded by mu
	file        *os.File
	offset      int64
	currentPath string
	partial     []byte // incomplete trailing line from last read
	mu          sync.Mutex
}

// NewTailer creates a Tailer that will watch dir for log files in the
// given format. pollInterval controls how often ReadLines checks for
// new data when driven by an external loop. maxLineLen silently
// discards any line exceeding that length. logFn is called for
// diagnostic messages (level, msg, args).
func NewTailer(
	dir, format string,
	pollInterval time.Duration,
	maxLineLen int,
	logFn func(string, string, ...any),
) *Tailer {
	return &Tailer{
		dir:          dir,
		format:       format,
		pollInterval: pollInterval,
		maxLineLen:   maxLineLen,
		logFn:        logFn,
	}
}

// Start opens the most recent log file and begins a background
// goroutine that periodically calls ReadLines until ctx is cancelled.
func (t *Tailer) Start(ctx context.Context) error {
	if err := t.openLatest(); err != nil {
		return err
	}
	go t.poll(ctx)
	return nil
}

// openLatest finds the newest log file, opens it, and seeks to the
// 1 MB lookback position (aligned to the next newline).
func (t *Tailer) openLatest() error {
	path, err := findLatestFile(t.dir, t.format)
	if err != nil {
		return err
	}
	return t.openAndSeek(path)
}

// openAndSeek opens path, seeks to (EOF - startupLookbackBytes), then
// aligns forward to the next newline so we never start mid-line.
func (t *Tailer) openAndSeek(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	seekPos := info.Size() - startupLookbackBytes
	if seekPos < 0 {
		seekPos = 0
	}
	if _, err := f.Seek(seekPos, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	offset := seekPos
	if seekPos > 0 {
		aligned, aerr := alignToNewline(f, seekPos)
		if aerr != nil {
			f.Close()
			return aerr
		}
		offset = aligned
	}
	t.file = f
	t.offset = offset
	t.currentPath = path
	t.partial = nil
	return nil
}

// alignToNewline reads forward byte-by-byte from pos until it hits
// '\n', then returns the offset immediately after that newline.
func alignToNewline(f *os.File, pos int64) (int64, error) {
	buf := make([]byte, 1)
	cur := pos
	for {
		n, err := f.Read(buf)
		if n == 1 {
			cur++
			if buf[0] == '\n' {
				return cur, nil
			}
		}
		if err == io.EOF {
			return cur, nil
		}
		if err != nil {
			return cur, err
		}
	}
}

// poll runs in a goroutine, calling ReadLines at pollInterval until
// ctx is done.
func (t *Tailer) poll(ctx context.Context) {
	ticker := time.NewTicker(t.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = t.ReadLines() // consumers retrieve lines via direct call
		}
	}
}

// ReadLines returns all new complete lines since the last call.
// It is safe to call from multiple goroutines.
func (t *Tailer) ReadLines() [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file == nil {
		return nil
	}
	if t.detectTruncation() {
		t.offset = 0
		t.partial = nil
		if _, err := t.file.Seek(0, io.SeekStart); err != nil {
			t.log("error", "seek after truncation: %v", err)
			return nil
		}
	}
	raw, err := t.readFromOffset()
	if err != nil {
		t.log("error", "read: %v", err)
		return nil
	}
	lines := t.splitLines(raw)
	t.maybeRotate()
	return lines
}

// detectTruncation returns true if the file was truncated (e.g.
// copytruncate rotation), meaning current size < our offset.
func (t *Tailer) detectTruncation() bool {
	info, err := t.file.Stat()
	if err != nil {
		return false
	}
	return info.Size() < t.offset
}

// readFromOffset reads all bytes from the current offset to EOF and
// advances offset accordingly.
func (t *Tailer) readFromOffset() ([]byte, error) {
	if _, err := t.file.Seek(t.offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(t.file)
	if err != nil {
		return nil, err
	}
	t.offset += int64(len(data))
	return data, nil
}

// splitLines splits raw bytes into complete lines, prepending any
// partial line from the previous call. An incomplete trailing chunk
// (no terminating newline) is saved in t.partial for next time.
func (t *Tailer) splitLines(raw []byte) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	// Prepend leftover partial from last read.
	if len(t.partial) > 0 {
		raw = append(t.partial, raw...)
		t.partial = nil
	}
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b != '\n' {
			continue
		}
		line := raw[start:i]
		start = i + 1
		if len(line) > t.maxLineLen {
			continue // silently discard oversized lines
		}
		// Strip trailing \r for Windows-style line endings.
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		lines = append(lines, cp)
	}
	// Anything after the last newline is a partial line.
	if start < len(raw) {
		t.partial = make([]byte, len(raw)-start)
		copy(t.partial, raw[start:])
	}
	return lines
}

// maybeRotate checks whether a newer log file exists. If so, it
// switches to it (starting from offset 0).
func (t *Tailer) maybeRotate() {
	newest, err := findLatestFile(t.dir, t.format)
	if err != nil || newest == t.currentPath {
		return
	}
	t.log("info", "rotating to newer log file: %s", newest)
	t.file.Close()
	if err := t.openFileAtStart(newest); err != nil {
		t.log("error", "open new log file %s: %v", newest, err)
	}
}

// openFileAtStart opens path at offset 0 (used for rotation, where
// we want to read the new file from the beginning).
func (t *Tailer) openFileAtStart(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	t.file = f
	t.offset = 0
	t.currentPath = path
	t.partial = nil
	return nil
}

// Stop closes the underlying file handle.
func (t *Tailer) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file != nil {
		t.file.Close()
		t.file = nil
	}
}

// log emits a diagnostic message via the configured logFn.
func (t *Tailer) log(level, msg string, args ...any) {
	if t.logFn != nil {
		t.logFn(level, msg, args...)
	}
}

// findLatestFile returns the path of the most recently modified file
// in dir that matches the expected extension for format.
func findLatestFile(dir, format string) (string, error) {
	ext := extensionForFormat(format)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	var matches []candidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ext {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		matches = append(matches, candidate{
			path:    filepath.Join(dir, e.Name()),
			modTime: info.ModTime(),
		})
	}
	if len(matches) == 0 {
		return "", os.ErrNotExist
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].modTime.After(matches[j].modTime)
	})
	return matches[0].path, nil
}

// extensionForFormat maps a PostgreSQL log_destination format name to
// the expected file extension. Falls back to ".log".
func extensionForFormat(format string) string {
	switch format {
	case "jsonlog":
		return ".json"
	case "csvlog":
		return ".csv"
	default:
		return ".log"
	}
}
