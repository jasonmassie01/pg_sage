package agenttools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// QueryID is a pg_stat_statements query id. It is a 64-bit integer that
// JSON numbers lose precision on in many clients, so it is written as a
// decimal string and read from a string or an exact integer number.
type QueryID int64

// MarshalJSON writes the id as a decimal string.
func (q QueryID) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(q), 10) + `"`), nil
}

// UnmarshalJSON reads a decimal string or an integer number, exactly.
func (q *QueryID) UnmarshalJSON(raw []byte) error {
	text := string(bytes.TrimSpace(raw))
	if len(text) > 0 && text[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%w: query id %s: %v", ErrInvalid, text, err)
		}
		text = s
	}
	id, err := ParseQueryID(text)
	if err != nil {
		return err
	}
	*q = id
	return nil
}

// ParseQueryID reads a decimal 64-bit query id.
func ParseQueryID(text string) (QueryID, error) {
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: query id %q is not a 64-bit integer", ErrInvalid, text)
	}
	return QueryID(n), nil
}

func toInt64s(ids []QueryID) []int64 {
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = int64(id)
	}
	return out
}

func toQueryIDs(ids []int64) []QueryID {
	out := make([]QueryID, len(ids))
	for i, id := range ids {
		out[i] = QueryID(id)
	}
	return out
}
