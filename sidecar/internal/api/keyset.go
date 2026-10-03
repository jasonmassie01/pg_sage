package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// List pagination (perf v1.8.3). Lists page by keyset: the cursor names
// the last row returned (its sort keys, its source and its id) and the
// next page starts strictly after it, through an index matching the
// list's filter and order. OFFSET is still accepted for older clients,
// bounded, and totals are counted up to a cap: no request reads a whole
// history table.
const (
	defaultListLimit = 50
	maxListLimit     = 200
	maxListOffset    = 1000
	maxListTotal     = 1000
	maxCursorBytes   = 2048
	maxCursorKeys    = 4
	cursorVersion    = 1
)

// listCursor is the position after the last row of a page.
type listCursor struct {
	V      int      `json:"v"`
	Sort   string   `json:"s"`
	Order  string   `json:"o"`
	Keys   []string `json:"k,omitempty"`
	Source string   `json:"src,omitempty"`
	ID     int64    `json:"id"`
}

// listPage is a request's page: Limit rows after Cursor, or after Offset
// rows when there is no cursor.
type listPage struct {
	Limit  int
	Offset int
	Cursor *listCursor
}

func encodeListCursor(c listCursor) string {
	c.V = cursorVersion
	raw, err := json.Marshal(c)
	if err != nil { // a struct of strings and ints always marshals
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeListCursor(raw string) (listCursor, error) {
	var c listCursor
	if len(raw) > maxCursorBytes {
		return c, errors.New("cursor is too long")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return c, fmt.Errorf("cursor is not valid: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("cursor is not valid: %w", err)
	}
	if c.V != cursorVersion || c.ID < 0 || len(c.Keys) > maxCursorKeys {
		return c, errors.New("cursor is not valid for this server")
	}
	return c, nil
}

// parseListPage reads limit, offset and cursor. A cursor must come from
// the same sort and order, and cannot be combined with an offset.
func parseListPage(q map[string][]string, sort, order string) (listPage, error) {
	p := listPage{Limit: parseIntDefault(firstVal(q, "limit"), defaultListLimit)}
	if p.Limit <= 0 {
		p.Limit = defaultListLimit
	}
	p.Limit = min(p.Limit, maxListLimit)
	if raw := firstVal(q, "offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return p, fmt.Errorf("offset %q is not a number", raw)
		}
		p.Offset = n
	}
	if p.Offset < 0 || p.Offset > maxListOffset {
		return p, fmt.Errorf("offset must be 0-%d; use the cursor from next_cursor "+
			"to page further", maxListOffset)
	}
	raw := firstVal(q, "cursor")
	if raw == "" {
		return p, nil
	}
	c, err := decodeListCursor(raw)
	if err != nil {
		return p, err
	}
	if p.Offset > 0 {
		return p, errors.New("use either cursor or offset, not both")
	}
	if c.Sort != sort || c.Order != order {
		return p, errors.New("cursor was issued for another sort order")
	}
	p.Cursor = &c
	return p, nil
}

// keysetBoundID is the id a source's rows are compared with when they tie
// the cursor on every sort key. Ties are ordered by source, then id: a
// source already passed in that order must skip its tied rows (0 when
// descending, MaxInt64 ascending), one not reached yet keeps them all.
func keysetBoundID(source, cursorSource string, id int64, desc bool) int64 {
	switch {
	case source == cursorSource:
		return id
	case (source < cursorSource) == desc:
		return 0
	default:
		return math.MaxInt64
	}
}

// keyKind is how a sort key is scanned, compared, encoded and cast.
type keyKind int

const (
	keyInt keyKind = iota
	keyTime
	keyText
	keyFloat
)

// sortKey is one ORDER BY expression of a list.
type sortKey struct {
	expr string
	kind keyKind
}

func (k sortKey) cast() string {
	switch k.kind {
	case keyInt:
		return "int8"
	case keyTime:
		return "timestamptz"
	case keyFloat:
		return "float8"
	default:
		return "text"
	}
}

// keyDest returns a scan destination for k.
func keyDest(k sortKey) any {
	switch k.kind {
	case keyInt:
		return new(int64)
	case keyTime:
		return new(time.Time)
	case keyFloat:
		return new(float64)
	default:
		return new(string)
	}
}

// keyValue dereferences a scanned destination.
func keyValue(dest any) any {
	switch v := dest.(type) {
	case *int64:
		return *v
	case *time.Time:
		return *v
	case *float64:
		return *v
	case *string:
		return *v
	}
	return nil
}

func encodeKey(v any) string {
	switch x := v.(type) {
	case int64:
		return strconv.FormatInt(x, 10)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case float64:
		switch {
		case math.IsInf(x, 1):
			return "Infinity"
		case math.IsInf(x, -1):
			return "-Infinity"
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return x
	}
	return ""
}

func compareKeys(a, b []any) int {
	for i := range a {
		if c := compareKey(a[i], b[i]); c != 0 {
			return c
		}
	}
	return 0
}

func compareKey(a, b any) int {
	switch x := a.(type) {
	case int64:
		return compareOrdered(x, b.(int64))
	case time.Time:
		return x.Compare(b.(time.Time))
	case float64:
		return compareOrdered(x, b.(float64))
	case string:
		return strings.Compare(x, b.(string))
	}
	return 0
}

func compareOrdered[T int64 | float64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// keysetSQL renders the ORDER BY, the "after the cursor" predicate (empty
// without a cursor) and the predicate's arguments, numbered from next.
func keysetSQL(keys []sortKey, desc bool, cur *listCursor, boundID int64,
	next int) (order, after string, args []any) {
	dir, op := "ASC", ">"
	if desc {
		dir, op = "DESC", "<"
	}
	exprs := make([]string, 0, len(keys)+1)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		exprs = append(exprs, k.expr)
		parts = append(parts, k.expr+" "+dir)
	}
	parts = append(parts, "id "+dir)
	order = " ORDER BY " + strings.Join(parts, ", ")
	if cur == nil {
		return order, "", nil
	}
	params := make([]string, 0, len(keys)+1)
	for i, k := range keys {
		params = append(params, fmt.Sprintf("$%d::%s", next+i, k.cast()))
		args = append(args, cur.Keys[i])
	}
	params = append(params, fmt.Sprintf("$%d::int8", next+len(keys)))
	args = append(args, boundID)
	after = fmt.Sprintf(" AND (%s, id) %s (%s)", strings.Join(exprs, ", "), op,
		strings.Join(params, ", "))
	return order, after, args
}
