package logwatch

import (
	"bytes"
	"encoding/csv"
	"strings"
)

// isCriticalLog reports whether a line with this severity and SQLSTATE must
// survive a log storm: PANIC or FATAL, or SQLSTATE class 53 (insufficient
// resources: disk full, out of memory, too many connections), 57P (operator
// intervention: shutdowns, cannot connect now), 58 (system error) or the
// corruption codes XX001 and XX002 (rca-06).
func isCriticalLog(level, sqlState string) bool {
	switch strings.ToUpper(level) {
	case "PANIC", "FATAL":
		return true
	}
	switch {
	case strings.HasPrefix(sqlState, "53"), strings.HasPrefix(sqlState, "57P"),
		strings.HasPrefix(sqlState, "58"):
		return true
	}
	return sqlState == "XX001" || sqlState == "XX002"
}

// isCriticalRecord sniffs one raw queued record for isCriticalLog without a
// full parse. It runs only when the line queue overflows. An unreadable
// record is not critical, so it is evicted like any other line.
func isCriticalRecord(format string, record []byte) bool {
	switch format {
	case "jsonlog":
		return isCriticalLog(jsonStringField(record, "error_severity"),
			jsonStringField(record, "state_code"))
	case "csvlog":
		reader := csv.NewReader(bytes.NewReader(record))
		reader.FieldsPerRecord = -1
		fields, err := reader.Read()
		if err != nil || len(fields) <= csvColSQLState {
			return false
		}
		return isCriticalLog(fields[csvColSeverity], fields[csvColSQLState])
	default:
		return false
	}
}

// jsonStringField returns the value of the first top-level-looking
// "key": "value" pair in a jsonlog line. A key quoted inside another string
// value cannot match: JSON escapes those quotes, so the key's closing quote
// would be preceded by a backslash. jsonlog writes error_severity and
// state_code before any free-text field, and their values never contain
// escapes, so the first match is the real one.
func jsonStringField(line []byte, key string) string {
	needle := []byte(`"` + key + `"`)
	i := bytes.Index(line, needle)
	if i < 0 {
		return ""
	}
	rest := bytes.TrimLeft(line[i+len(needle):], " \t")
	if len(rest) == 0 || rest[0] != ':' {
		return ""
	}
	rest = bytes.TrimLeft(rest[1:], " \t")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	end := bytes.IndexByte(rest[1:], '"')
	if end < 0 {
		return ""
	}
	return string(rest[1 : 1+end])
}
