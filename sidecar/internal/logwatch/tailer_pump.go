package logwatch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"
)

// maxTailerQueueSize bounds unread complete lines between the single
// offset-owning pump and consumers. Overflow evicts the oldest non-critical
// lines first (isCriticalRecord), so a PANIC or disk-full line survives a
// log storm; only a queue full of critical lines loses its oldest ones.
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
	t.criticalHead = 0
	return lines
}

// TakeDropped returns how many queued lines overflow evicted since the
// last call, and resets the count.
func (t *Tailer) TakeDropped() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.dropped
	t.dropped = 0
	return n
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
	overflow := len(t.queue) - maxTailerQueueSize
	if overflow <= 0 {
		return
	}
	critical := t.evictOverflow(overflow)
	t.dropped += overflow
	t.log("warn", "log line queue overflow: dropped %d lines (%d critical); "+
		"critical lines are kept first", overflow, critical)
}

// evictOverflow removes overflow queued lines, oldest non-critical first,
// preserving order. criticalHead counts the leading lines already known to
// be critical, so each line is sniffed about once per storm. It returns how
// many critical lines had to go because nothing else was left. Caller holds
// t.mu.
func (t *Tailer) evictOverflow(overflow int) int {
	queued := len(t.queue)
	kept := t.queue[:t.criticalHead]
	i, evicted := t.criticalHead, 0
	for ; i < queued && evicted < overflow; i++ {
		if isCriticalRecord(t.format, t.queue[i]) {
			kept = append(kept, t.queue[i])
			continue
		}
		evicted++
	}
	t.criticalHead = len(kept)
	kept = append(kept, t.queue[i:]...)
	criticalEvicted := overflow - evicted
	if criticalEvicted > 0 {
		copy(kept, kept[criticalEvicted:])
		kept = kept[:len(kept)-criticalEvicted]
		t.criticalHead -= criticalEvicted
	}
	clear(t.queue[len(kept):queued])
	t.queue = kept
	return criticalEvicted
}
