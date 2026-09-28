package probes

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxTextRunes bounds one rendered text value (application names,
// relation names): the text is untrusted data, not instructions.
const maxTextRunes = 64

// FormatValue renders one value the same way everywhere evidence is
// shown, so a number quoted from evidence matches it exactly. Floats
// keep at most two decimals; NaN is "unknown".
func FormatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case float64:
		return formatFloat(x)
	case float32:
		return formatFloat(float64(x))
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return x.UTC().Truncate(time.Second).Format(time.RFC3339)
	case string:
		return truncateText(x)
	case json.Number:
		return x.String()
	default:
		return truncateText(fmt.Sprint(x))
	}
}

func formatFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "unknown"
	}
	s := strconv.FormatFloat(f, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" {
		return "0"
	}
	return s
}

func truncateText(s string) string {
	if utf8.RuneCountInString(s) <= maxTextRunes {
		return s
	}
	return string([]rune(s)[:maxTextRunes]) + "..."
}

// Text renders the result as bounded evidence text: a header line and
// at most maxRows rows, naming omitted rows and cap truncation.
func (r Result) Text(maxRows int) string {
	head := fmt.Sprintf("%s %s %s", r.ProbeID, r.Version, r.Status)
	if !r.Status.Usable() {
		return head + ": " + r.Reason
	}
	lines := []string{fmt.Sprintf("%s, %d rows", head, len(r.Rows))}
	cols := r.columns()
	for i, row := range r.Rows {
		if i == maxRows {
			lines = append(lines, fmt.Sprintf("(%d more rows not shown)",
				len(r.Rows)-maxRows))
			break
		}
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, c+"="+FormatValue(row[c]))
		}
		lines = append(lines, fmt.Sprintf("[%d] %s", i+1, strings.Join(parts, " ")))
	}
	if r.Truncated {
		lines = append(lines, "(truncated at the probe cap)")
	}
	return strings.Join(lines, "\n")
}

func (r Result) columns() []string {
	if len(r.Columns) > 0 {
		return r.Columns
	}
	seen := map[string]bool{}
	var cols []string
	for _, row := range r.Rows {
		for k := range row {
			if !seen[k] {
				seen[k] = true
				cols = append(cols, k)
			}
		}
	}
	sort.Strings(cols)
	return cols
}

// Payload is the result as JSON (the immutable evidence payload).
func (r Result) Payload() ([]byte, error) { return json.Marshal(r) }
