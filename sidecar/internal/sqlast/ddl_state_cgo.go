//go:build cgo

package sqlast

import (
	"math"
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// The session timeouts InspectDDL tracks through a script.
const (
	gucLockTimeout      = "lock_timeout"
	gucStatementTimeout = "statement_timeout"
)

// timeoutValues maps a tracked GUC to its value in milliseconds; a nil
// value means the server default.
type timeoutValues map[string]*int64

func (v timeoutValues) clone() timeoutValues {
	out := make(timeoutValues, len(v))
	for k, val := range v {
		out[k] = val
	}
	return out
}

// scriptState follows the explicit transaction block and the tracked
// timeouts as a script runs: session values, SET LOCAL values that end
// with the block, and the session values a ROLLBACK restores.
type scriptState struct {
	inBlock bool
	session timeoutValues
	local   timeoutValues
	atBegin timeoutValues
}

func newScriptState() *scriptState {
	return &scriptState{session: timeoutValues{}, local: timeoutValues{}}
}

func (s *scriptState) effective(name string) *int64 {
	value, ok := s.local[name]
	if !ok {
		value = s.session[name]
	}
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

// annotate records the state in which stmt starts.
func (s *scriptState) annotate(stmt *DDLStatement) {
	stmt.InTransactionBlock = s.inBlock
	stmt.LockTimeoutMs = s.effective(gucLockTimeout)
	stmt.StatementTimeoutMs = s.effective(gucStatementTimeout)
}

// apply moves the state past stmt.
func (s *scriptState) apply(stmt *pg_query.Node) {
	switch {
	case stmt.GetTransactionStmt() != nil:
		s.applyTransaction(stmt.GetTransactionStmt())
	case stmt.GetVariableSetStmt() != nil:
		s.applySet(stmt.GetVariableSetStmt())
	}
}

func (s *scriptState) applyTransaction(txn *pg_query.TransactionStmt) {
	switch txn.GetKind() {
	case pg_query.TransactionStmtKind_TRANS_STMT_BEGIN,
		pg_query.TransactionStmtKind_TRANS_STMT_START:
		if !s.inBlock {
			s.inBlock = true
			s.atBegin = s.session.clone()
		}
	case pg_query.TransactionStmtKind_TRANS_STMT_COMMIT,
		pg_query.TransactionStmtKind_TRANS_STMT_PREPARE:
		s.endBlock(txn.GetChain())
	case pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK:
		if s.inBlock {
			s.session = s.atBegin
		}
		s.endBlock(txn.GetChain())
	}
}

// endBlock ends the block (SET LOCAL values lapse); AND CHAIN opens the
// next one at once.
func (s *scriptState) endBlock(chain bool) {
	if !s.inBlock {
		return
	}
	s.local = timeoutValues{}
	s.inBlock = chain
	s.atBegin = s.session.clone()
}

func (s *scriptState) applySet(set *pg_query.VariableSetStmt) {
	name := strings.ToLower(set.GetName())
	var value *int64
	switch set.GetKind() {
	case pg_query.VariableSetKind_VAR_RESET_ALL:
		s.session, s.local = timeoutValues{}, timeoutValues{}
		return
	case pg_query.VariableSetKind_VAR_SET_VALUE:
		parsed, ok := durationMs(set.GetArgs())
		if !ok {
			return
		}
		value = &parsed
	case pg_query.VariableSetKind_VAR_SET_DEFAULT, pg_query.VariableSetKind_VAR_RESET:
	default:
		return
	}
	if name != gucLockTimeout && name != gucStatementTimeout {
		return
	}
	switch {
	case set.GetIsLocal() && s.inBlock:
		s.local[name] = value
	case !set.GetIsLocal():
		s.session[name] = value
		delete(s.local, name)
	}
}

// timeUnits are PostgreSQL's time units for GUCs, in milliseconds.
var timeUnits = map[string]float64{
	"us": 0.001, "ms": 1, "s": 1000, "min": 60000, "h": 3600000, "d": 86400000,
}

// durationMs reads a timeout GUC value: a bare number is milliseconds, a
// string may carry a unit. Invalid or negative values report false, as the
// server would refuse them.
func durationMs(args []*pg_query.Node) (int64, bool) {
	if len(args) != 1 || args[0].GetAConst() == nil {
		return 0, false
	}
	c := args[0].GetAConst()
	var number float64
	switch {
	case c.GetIval() != nil:
		number = float64(c.GetIval().GetIval())
	case c.GetFval() != nil:
		f, err := strconv.ParseFloat(c.GetFval().GetFval(), 64)
		if err != nil {
			return 0, false
		}
		number = f
	case c.GetSval() != nil:
		return durationText(c.GetSval().GetSval())
	default:
		return 0, false
	}
	return roundMs(number, 1)
}

func durationText(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	split := strings.IndexFunc(text, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.' && r != '-' && r != '+'
	})
	numberText, unit := text, "ms"
	if split >= 0 {
		numberText, unit = text[:split], strings.TrimSpace(text[split:])
	}
	scale, ok := timeUnits[unit]
	number, err := strconv.ParseFloat(numberText, 64)
	if !ok || err != nil {
		return 0, false
	}
	return roundMs(number, scale)
}

func roundMs(number, scale float64) (int64, bool) {
	ms := math.Round(number * scale)
	if ms < 0 || ms > math.MaxInt32 || math.IsNaN(ms) {
		return 0, false
	}
	return int64(ms), true
}

func settingOf(set *pg_query.VariableSetStmt) *Setting {
	out := &Setting{Name: strings.ToLower(set.GetName()), Local: set.GetIsLocal()}
	switch set.GetKind() {
	case pg_query.VariableSetKind_VAR_RESET, pg_query.VariableSetKind_VAR_RESET_ALL,
		pg_query.VariableSetKind_VAR_SET_DEFAULT:
		out.Reset = true
		return out
	}
	values := make([]string, 0, len(set.GetArgs()))
	for _, arg := range set.GetArgs() {
		values = append(values, constText(arg))
	}
	out.Value = strings.Join(values, ", ")
	return out
}

// constText renders a SET argument as written.
func constText(arg *pg_query.Node) string {
	c := arg.GetAConst()
	switch {
	case c == nil:
		return deparseExpr(arg)
	case c.GetSval() != nil:
		return c.GetSval().GetSval()
	case c.GetIval() != nil:
		return strconv.Itoa(int(c.GetIval().GetIval()))
	case c.GetFval() != nil:
		return c.GetFval().GetFval()
	case c.GetBoolval() != nil:
		return strconv.FormatBool(c.GetBoolval().GetBoolval())
	}
	return deparseExpr(arg)
}
