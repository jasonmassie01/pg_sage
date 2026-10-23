// Package pgaudit correlates pgaudit's records in the server log to pg_sage
// principals and actions (E2, spec §6.17, AU-2 database-side
// corroboration). It reads only what pg_sage can already read: the log
// entries internal/logwatch parses. Brokered agent connections carry
// application_name 'pg_sage agent:<principal>[:<action>]'; pg_sage's own
// statements are linked to the action whose SQL they executed; direct-lane
// agent roles are recognised by their sage_agent_ name.
package pgaudit

import (
	"encoding/csv"
	"strconv"
	"strings"
)

// auditPrefix starts every pgaudit log message.
const auditPrefix = "AUDIT: "

// Record is one parsed pgaudit message.
type Record struct {
	AuditType      string // SESSION or OBJECT
	StatementID    int64
	SubstatementID int64
	Class          string // READ, WRITE, FUNCTION, ROLE, DDL, MISC, MISC_SET
	Command        string
	ObjectType     string
	ObjectName     string
	Statement      string
}

// Parse reads a pgaudit message: "AUDIT: <type>,<statement id>,
// <substatement id>,<class>,<command>,<object type>,<object name>,
// <statement>[,<parameter>]" in CSV quoting.
func Parse(message string) (Record, bool) {
	body, ok := strings.CutPrefix(message, auditPrefix)
	if !ok || body == "" {
		return Record{}, false
	}
	r := csv.NewReader(strings.NewReader(body))
	r.FieldsPerRecord = -1
	r.LazyQuotes = false
	fields, err := r.Read()
	if err != nil || len(fields) < 8 {
		return Record{}, false
	}
	if fields[0] != "SESSION" && fields[0] != "OBJECT" {
		return Record{}, false
	}
	stmt, err1 := strconv.ParseInt(fields[1], 10, 64)
	sub, err2 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil {
		return Record{}, false
	}
	return Record{AuditType: fields[0], StatementID: stmt, SubstatementID: sub,
		Class: fields[3], Command: fields[4], ObjectType: fields[5],
		ObjectName: fields[6], Statement: fields[7]}, true
}
