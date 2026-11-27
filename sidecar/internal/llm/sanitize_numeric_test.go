package llm

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// llm-09: numbers of 6 or more digits in comparison contexts (card numbers,
// SSNs, account ids) are redacted before text reaches a model; short
// numbers and numbers outside comparisons keep the SQL readable.

func TestRedactNumericLiteralsComparisons(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT * FROM cards WHERE pan = 4111111111111111 LIMIT 10",
			"SELECT * FROM cards WHERE pan = ? LIMIT 10"},
		{"WHERE id IN (123456, 7)", "WHERE id IN (?, 7)"},
		{"WHERE id in (123456,654321)", "WHERE id in (?,?)"},
		{"Key (id)=(4111111111111111) already exists.", "Key (id)=(?) already exists."},
		{"Key (a, b)=(123456, 42)", "Key (a, b)=(?, 42)"},
		{"WHERE id = ANY(ARRAY[123456, 99, 7654321])", "WHERE id = ANY(ARRAY[?, 99, ?])"},
		{"WHERE x BETWEEN 1000000 AND 2000000", "WHERE x BETWEEN ? AND ?"},
		{"WHERE x NOT BETWEEN 12 AND 2000000", "WHERE x NOT BETWEEN 12 AND ?"},
		{"WHERE a <> 123456 AND b != 123456 AND c < 123456",
			"WHERE a <> ? AND b != ? AND c < ?"},
		{"WHERE a > 123456 OR a <= 123456 OR a >= 123456", "WHERE a > ? OR a <= ? OR a >= ?"},
		{"(ssn = 123456789)", "(ssn = ?)"},
		{"WHERE 123456789 = ssn", "WHERE ? = ssn"},
		{"WHERE pan=4111111111111111::bigint", "WHERE pan=?::bigint"},
	}
	for _, c := range cases {
		if got := RedactNumericLiterals(c.in); got != c.want {
			t.Errorf("RedactNumericLiterals(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestRedactNumericLiteralsKeepsNonComparisonNumbers(t *testing.T) {
	for _, in := range []string{
		"SELECT a FROM t LIMIT 1000000 OFFSET 2000000",
		"SELECT 1234567 AS answer",
		"CREATE TABLE t (amount numeric(123456, 2))",
		"SELECT x::varchar(1000000) FROM t",
		"SELECT f(1234567) FROM t",
		"UPDATE t SET n = n + 1",
		"SELECT * FROM t1234567 WHERE c1234567 = 5",
		"WHERE a = $1234567",
	} {
		if got := RedactNumericLiterals(in); got != in {
			t.Errorf("RedactNumericLiterals(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestRedactNumericLiteralsDigitBoundary(t *testing.T) {
	cases := []struct{ in, want string }{
		{"WHERE id = 12345", "WHERE id = 12345"},
		{"WHERE id = 123456", "WHERE id = ?"},
		{"WHERE id = -123456", "WHERE id = ?"},
		{"WHERE id = -12345", "WHERE id = -12345"},
		{"WHERE amt = 1234567.89", "WHERE amt = ?"},
		{"WHERE amt = 1234.5", "WHERE amt = 1234.5"},
		{"WHERE amt = 1.5e10", "WHERE amt = 1.5e10"},
		{"WHERE amt = 12345.6", "WHERE amt = ?"},
		{"WHERE n = 1_000_000", "WHERE n = ?"},
		{"WHERE n = +123456", "WHERE n = ?"},
	}
	for _, c := range cases {
		if got := RedactNumericLiterals(c.in); got != c.want {
			t.Errorf("RedactNumericLiterals(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactNumericLiteralsInvalidInputUnchanged(t *testing.T) {
	for _, in := range []string{
		`SELECT "t123456" FROM x WHERE "c" = "t123456"`,
		`WHERE note = 'unterminated 123456`,
		`WHERE body = $$ x = 123456 $$`,
		`WHERE s = 'a = 123456'`,
		`WHERE "id = 123456" = 1`,
		"",
		"=",
		"= -",
		"IN (",
		"BETWEEN",
	} {
		if got := RedactNumericLiterals(in); got != in {
			t.Errorf("RedactNumericLiterals(%q) = %q, want unchanged", in, got)
		}
	}
}

func TestSanitizeForLLMRedactsNumbersAndKeepsEverythingElse(t *testing.T) {
	got := SanitizeForLLM("/* note */ SELECT * FROM \"t123456\" WHERE pan = 4111111111111111 " +
		"AND name = 'bob' AND body = $x$ secret $x$ LIMIT 10 -- tail")
	want := "  SELECT * FROM \"t123456\" WHERE pan = ? AND name = '?' AND body = $?$ LIMIT 10 "
	if got != want {
		t.Fatalf("SanitizeForLLM\n got %q\nwant %q", got, want)
	}
}

func TestSanitizeForLLMPlanJSONRedactsFilterNumbersOnly(t *testing.T) {
	plan := `[{"Plan": {"Node Type": "Seq Scan", "Plan Rows": 1234567, ` +
		`"Total Cost": 98765432.10, "Filter": "(ssn = 123456789)", ` +
		`"Index Cond": "(id = ANY ('{1,2}'::bigint[]))"}}]`
	got := SanitizeForLLM(plan)
	if strings.Contains(got, "123456789") {
		t.Fatalf("Filter number survived: %s", got)
	}
	for _, keep := range []string{`"Plan Rows":1234567`, `"Total Cost":98765432.10`} {
		if !strings.Contains(got, keep) {
			t.Errorf("sanitized plan lost %s: %s", keep, got)
		}
	}
	var decoded []map[string]map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("sanitized plan is not JSON: %v\n%s", err, got)
	}
	if f := decoded[0]["Plan"]["Filter"]; f != "(ssn = ?)" {
		t.Errorf("Filter = %q, want (ssn = ?)", f)
	}
}

func TestSanitizeForLLMTruncatedPlanRedactsNumbers(t *testing.T) {
	got := SanitizeForLLM(`[{"Plan": {"Plan Rows": 1234567, "Filter": "(pan = 4111111111111111)`)
	if strings.Contains(got, "4111111111111111") {
		t.Errorf("truncated plan leaked the number: %s", got)
	}
	if !strings.Contains(got, `"Plan Rows": 1234567`) {
		t.Errorf("truncated plan lost Plan Rows: %s", got)
	}
}

func TestSanitizeForLLMNumericRedactionOff(t *testing.T) {
	inputs := []string{
		"SELECT * FROM t WHERE pan = 4111111111111111 AND s = 'x' /* c */",
		`[{"Plan": {"Plan Rows": 1234567, "Filter": "(ssn = 123456789)"}}]`,
		`[{"Plan": {"Filter": "(pan = 4111111111111111)`,
	}
	defer SetRedactNumericLiterals(true)
	SetRedactNumericLiterals(false)
	if RedactNumericLiteralsEnabled() {
		t.Fatal("RedactNumericLiteralsEnabled() = true after turning it off")
	}
	for _, in := range inputs {
		want := legacySanitize(in)
		if got := SanitizeForLLM(in); got != want {
			t.Errorf("off: SanitizeForLLM(%q)\n got %q\nwant %q (pre-llm-09 output)", in, got, want)
		}
	}
	SetRedactNumericLiterals(true)
	if got := SanitizeForLLM(inputs[0]); strings.Contains(got, "4111111111111111") {
		t.Errorf("back on: number survived: %s", got)
	}
}

// legacySanitize is SanitizeForLLM as it was before llm-09: comments
// stripped and string and dollar literals redacted, numbers kept.
func legacySanitize(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var v any
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		if dec.Decode(&v) == nil && !dec.More() {
			var b strings.Builder
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(legacyJSON(v))
			return strings.TrimSuffix(b.String(), "\n")
		}
		return redactLiterals(stripComments(text, false), false)
	}
	return RedactSQLLiterals(StripSQLComments(text))
}

func legacyJSON(v any) any {
	switch x := v.(type) {
	case string:
		return RedactSQLLiterals(StripSQLComments(x))
	case []any:
		for i := range x {
			x[i] = legacyJSON(x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = legacyJSON(x[k])
		}
	}
	return v
}

func TestRedactNumericLiteralsDefaultOn(t *testing.T) {
	if !RedactNumericLiteralsEnabled() {
		t.Fatal("numeric redaction is off by default; llm.redact_numeric_literals defaults true")
	}
}

func TestRedactLiteralsForLLMLogMessage(t *testing.T) {
	got := RedactLiteralsForLLM(`duplicate key: Key (pan)=(4111111111111111) note 'x' -- keep`)
	want := `duplicate key: Key (pan)=(?) note '?' -- keep`
	if got != want {
		t.Fatalf("RedactLiteralsForLLM = %q, want %q", got, want)
	}
	defer SetRedactNumericLiterals(true)
	SetRedactNumericLiterals(false)
	if got := RedactLiteralsForLLM(`Key (pan)=(4111111111111111)`); got !=
		`Key (pan)=(4111111111111111)` {
		t.Fatalf("off: RedactLiteralsForLLM = %q", got)
	}
}

// Concurrent access: the switch is process-wide and hot-reloadable, so a
// reload racing with prompt builders must never yield a half-redacted
// string: each call sees on or off.
func TestSanitizeForLLMNumericSwitchConcurrent(t *testing.T) {
	defer SetRedactNumericLiterals(true)
	in := "WHERE a = 123456 AND b = 654321"
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(on bool) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				SetRedactNumericLiterals(on)
			}
		}(i%2 == 0)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				got := SanitizeForLLM(in)
				if got != in && got != "WHERE a = ? AND b = ?" {
					t.Errorf("mixed redaction: %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
