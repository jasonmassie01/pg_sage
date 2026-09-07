package logwatch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"
)

// maxTailerQueueSize bounds unread complete lines between the single
// offset-owning pump and consumers. Raw lines have no severity yet, so
// overflow evicts the oldest line.
const maxTailerQueueSize = 10000

const tailerCheckpointBytes = 64

const (
	tailerReadChunkBytes   = 256 * 1024
	tailerMaxChunksPerPump = 64
)

func (t *Tailer) poll(
	ctx context.Context,
	requests <-chan chan struct{},
	stopCh <-chan struct{},
	doneCh chan struct{},
) {
	interval := t.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer t.finishPump(doneCh)
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopCh:
			return
		case <-ticker.C:
			t.pump()
		case ack := <-requests:
			t.pump()
			close(ack)
		}
	}
}

func (t *Tailer) finishPump(doneCh chan struct{}) {
	t.mu.Lock()
	if t.doneCh == doneCh {
		t.started = false
	}
	t.mu.Unlock()
	close(doneCh)
}

// ReadLines requests a read from the offset-owning pump, then drains each
// queued complete line once. Before Start or after cancellation, the caller
// temporarily owns pumping under the same mutex.
func (t *Tailer) ReadLines() [][]byte {
	if !t.requestPump() {
		t.pump()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := t.queue
	t.queue = nil
	return lines
}

func (t *Tailer) requestPump() bool {
	t.mu.Lock()
	if !t.started {
		t.mu.Unlock()
		return false
	}
	requests := t.pumpRequests
	doneCh := t.doneCh
	t.mu.Unlock()

	ack := make(chan struct{})
	select {
	case requests <- ack:
	case <-doneCh:
		return false
	}
	select {
	case <-ack:
		return true
	case <-doneCh:
		return false
	}
}

func (t *Tailer) pump() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.pumpLocked(); err != nil {
		t.log("error", "read: %v", err)
	}
}

func (t *Tailer) pumpLocked() error {
	if t.file == nil {
		return nil
	}
	if t.detectTruncation() {
		t.offset = 0
		t.partial = nil
		t.discardUntilNewline = false
		t.checkpoint = nil
		t.checkpointOffset = 0
		if _, err := t.file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek after truncation: %w", err)
		}
	}
	reachedEOF := false
	for i := 0; i < tailerMaxChunksPerPump; i++ {
		raw, atEOF, err := t.readChunkFromOffset()
		if err != nil {
			return err
		}
		t.enqueue(t.splitLines(raw))
		if atEOF {
			reachedEOF = true
			break
		}
	}
	t.captureCheckpoint()
	if reachedEOF {
		t.maybeRotate()
	}
	return nil
}

func (t *Tailer) detectTruncation() bool {
	info, err := t.file.Stat()
	if err != nil {
		return false
	}
	if info.Size() < t.offset {
		return true
	}
	if len(t.checkpoint) == 0 || t.checkpointOffset != t.offset {
		return false
	}
	start := t.checkpointOffset - int64(len(t.checkpoint))
	current := make([]byte, len(t.checkpoint))
	n, readErr := t.file.ReadAt(current, start)
	if readErr != nil && readErr != io.EOF {
		return false
	}
	return n != len(t.checkpoint) || !bytes.Equal(current, t.checkpoint)
}

func (t *Tailer) readChunkFromOffset() ([]byte, bool, error) {
	if _, err := t.file.Seek(t.offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	data := make([]byte, tailerReadChunkBytes)
	n, err := io.ReadFull(t.file, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false, err
	}
	t.offset += int64(n)
	return data[:n], err == io.EOF || err == io.ErrUnexpectedEOF, nil
}

func (t *Tailer) captureCheckpoint() {
	if t.file == nil || t.offset <= 0 {
		t.checkpoint = nil
		t.checkpointOffset = t.offset
		return
	}
	size := int64(tailerCheckpointBytes)
	if t.offset < size {
		size = t.offset
	}
	checkpoint := make([]byte, int(size))
	n, err := t.file.ReadAt(checkpoint, t.offset-size)
	if err != nil && err != io.EOF {
		t.checkpoint = nil
		t.checkpointOffset = t.offset
		return
	}
	t.checkpoint = checkpoint[:n]
	t.checkpointOffset = t.offset
}

func (t *Tailer) enqueue(lines [][]byte) {
	if len(lines) == 0 {
		return
	}
	t.queue = append(t.queue, lines...)
	if overflow := len(t.queue) - maxTailerQueueSize; overflow > 0 {
		copy(t.queue, t.queue[overflow:])
		t.queue = t.queue[:maxTailerQueueSize]
		t.log("warn", "log line queue overflow: dropped %d oldest lines", overflow)
	}
}
