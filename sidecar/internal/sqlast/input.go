package sqlast

import (
	"errors"
	"fmt"
)

// MaxInputBytes is the largest SQL text InspectDDL, References, Fingerprint
// and ReadShape accept. Larger inputs are rejected before the parser runs.
const MaxInputBytes = 100_000

var (
	// ErrInputTooLarge reports an input over MaxInputBytes. It always comes
	// wrapped together with ErrRejected.
	ErrInputTooLarge = errors.New("SQL input is over the size limit")
	// ErrUnsupported reports a statement ReadShape has no read form for
	// (UPDATE ... FROM, DELETE ... USING, MERGE, DDL and the like).
	ErrUnsupported = errors.New("statement has no read shape")
)

// ParseError is libpg_query's refusal of an input. It unwraps to
// ErrRejected, so errors.Is(err, ErrRejected) holds.
type ParseError struct {
	// Message is the parser's own message, e.g. `syntax error at or near "x"`.
	Message string
	// Offset is the byte offset into the input of the failing position, or
	// -1 when the parser reported none.
	Offset int
	// Line is the 1-based line of Offset, or 0 when Offset is -1.
	Line int
}

func (e *ParseError) Error() string {
	if e.Offset < 0 {
		return fmt.Sprintf("%v: parse error: %s", ErrRejected, e.Message)
	}
	return fmt.Sprintf("%v: parse error at offset %d (line %d): %s",
		ErrRejected, e.Offset, e.Line, e.Message)
}

// Unwrap lets errors.Is match ErrRejected.
func (e *ParseError) Unwrap() error { return ErrRejected }
