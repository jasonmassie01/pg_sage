package logwatch

import (
	"bytes"
	"context"
	"fmt"
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
	// discardUntilNewline is set when partial exceeded maxLineLen and
	// was dropped; subsequent bytes are discarded until a '\n' marks a
	// safe resync point. Prevents unbounded partial-buffer growth when
	// a producer writes a huge line or never terminates one.
	discardUntilNewline bool
	queue               [][]byte
	checkpoint          []byte
	checkpointOffset    int64
	started             bool
	pumpRequests        chan chan struct{}
	stopCh              chan struct{}
	doneCh              chan struct{}
	mu                  sync.Mutex
}

// NewTailer creates a Tailer that will watch dir for log files in the
// given format. pollInterval controls how often the single offset-owning
// pump checks for new data. maxLineLen silently discards any line exceeding
// that length. logFn is called for diagnostic messages (level, msg, args).
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

// Start opens the most recent log file and begins the background pump that
// fills the bounded line queue until ctx is cancelled.
func (t *Tailer) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.file != nil {
		t.mu.Unlock()
		return fmt.Errorf("tailer already started")
	}
	if err := t.openLatest(); err != nil {
		t.mu.Unlock()
		return err
	}
	if err := t.pumpLocked(); err != nil {
		_ = t.file.Close()
		t.file = nil
		t.mu.Unlock()
		return err
	}
	requests := make(chan chan struct{})
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	t.pumpRequests = requests
	t.stopCh = stopCh
	t.doneCh = doneCh
	t.started = true
	t.mu.Unlock()
	go t.poll(ctx, requests, stopCh, doneCh)
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
		_ = f.Close()
		return err
	}
	seekPos := info.Size() - startupLookbackBytes
	if seekPos < 0 {
		seekPos = 0
	}
	if _, err := f.Seek(seekPos, io.SeekStart); err != nil {
		_ = f.Close()
		return err
	}
	offset := seekPos
	if seekPos > 0 {
		aligned, aerr := alignToNewline(f, seekPos)
		if aerr != nil {
			_ = f.Close()
			return aerr
		}
		offset = aligned
	}
	t.file = f
	t.offset = offset
	t.currentPath = path
	t.partial = nil
	t.discardUntilNewline = false
	t.queue = nil
	t.captureCheckpoint()
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

// splitLines splits raw bytes into complete lines, prepending any
// partial line from the previous call. An incomplete trailing chunk
// (no terminating newline) is saved in t.partial for next time,
// but bounded at maxLineLen — once exceeded, the partial is dropped
// and subsequent bytes are discarded until the next newline to resync.
func (t *Tailer) splitLines(raw []byte) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	// If we are in discard-until-newline mode (previous partial
	// overflowed), skip bytes up to and including the next '\n'.
	if t.discardUntilNewline {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			return nil // still no newline — drop everything
		}
		raw = raw[i+1:]
		t.discardUntilNewline = false
		if len(raw) == 0 {
			return nil
		}
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
		remain := raw[start:]
		if len(remain) > t.maxLineLen {
			// Oversized partial: drop it and resync at the next
			// newline to prevent unbounded buffer growth.
			t.log("warn",
				"partial log line exceeded max_line_len (%d bytes); discarding to next newline",
				len(remain))
			t.partial = nil
			t.discardUntilNewline = true
		} else {
			t.partial = make([]byte, len(remain))
			copy(t.partial, remain)
		}
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
	oldFile := t.file
	if err := t.openFileAtStart(newest); err != nil {
		t.log("error", "open new log file %s: %v", newest, err)
		return
	}
	_ = oldFile.Close()
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
	t.discardUntilNewline = false
	t.checkpoint = nil
	t.checkpointOffset = 0
	return nil
}

// Stop closes the underlying file handle.
func (t *Tailer) Stop() {
	t.mu.Lock()
	stopCh := t.stopCh
	doneCh := t.doneCh
	if stopCh != nil {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	}
	t.mu.Unlock()
	if doneCh != nil {
		<-doneCh
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.file != nil {
		_ = t.file.Close()
		t.file = nil
	}
	t.queue = nil
	t.partial = nil
	t.checkpoint = nil
	t.pumpRequests = nil
	t.stopCh = nil
	t.doneCh = nil
	t.started = false
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
