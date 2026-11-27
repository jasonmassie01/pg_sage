package logwatch

import (
	"bytes"
	"regexp"
)

// pgTimestampPrefixLen is len("2006-01-02 15:04:05"), the prefix every
// csvlog record starts with.
const pgTimestampPrefixLen = 19

// recordEnd returns the index of the newline that terminates the record
// starting at start, or -1 if the record is incomplete. Non-CSV formats
// are newline-delimited. csvlog records may contain newlines inside
// quoted fields (multi-line DETAIL, statements), so a newline only ends a
// record outside quotes. A newline inside quotes that is immediately
// followed by a log timestamp also ends the record: that resynchronises
// quote parity when reading started mid-record (startup lookback,
// oversized-record discard) instead of mis-splitting the rest of the file.
func (t *Tailer) recordEnd(raw []byte, start int) int {
	if t.format != "csvlog" {
		i := bytes.IndexByte(raw[start:], '\n')
		if i < 0 {
			return -1
		}
		return start + i
	}
	inQuote := false
	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			inQuote = !inQuote
		case '\n':
			if !inQuote || looksLikeRecordStart(raw[i+1:]) {
				return i
			}
		}
	}
	return -1
}

// looksLikeRecordStart reports whether b begins with a PostgreSQL log
// timestamp "YYYY-MM-DD HH:MM:SS".
func looksLikeRecordStart(b []byte) bool {
	if len(b) < pgTimestampPrefixLen {
		return false
	}
	for i := 0; i < pgTimestampPrefixLen; i++ {
		c := b[i]
		switch i {
		case 4, 7:
			if c != '-' {
				return false
			}
		case 10:
			if c != ' ' {
				return false
			}
		case 13, 16:
			if c != ':' {
				return false
			}
		default:
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// PlanRecordMaxBytes is the largest auto_explain plan record kept: the
// size the plan parser accepts (rca-04). Other records keep
// logwatch.max_line_len_bytes.
const PlanRecordMaxBytes = 1 << 20

// planHeaderWindow is how far into a record the auto_explain header is
// looked for: past the csvlog columns or jsonlog keys before the message.
const planHeaderWindow = 8 << 10

var planRecordHeader = regexp.MustCompile(`duration: [0-9]+(?:\.[0-9]+)? ms\s+plan:`)

// recordLimit is the size limit for the record starting with rec: an
// auto_explain plan record ("duration: ... ms  plan:") may reach
// PlanRecordMaxBytes, any other one maxLineLen.
func (t *Tailer) recordLimit(rec []byte) int {
	if t.maxLineLen >= PlanRecordMaxBytes {
		return t.maxLineLen
	}
	head := rec[:min(len(rec), planHeaderWindow)]
	if planRecordHeader.Match(head) {
		return PlanRecordMaxBytes
	}
	return t.maxLineLen
}

// TakeOversized returns how many records were dropped for their size
// since the last call, and resets the count.
func (t *Tailer) TakeOversized() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.oversized
	t.oversized = 0
	return n
}
