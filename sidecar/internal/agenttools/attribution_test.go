package agenttools

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests in this file: ParseSQLCommenter and QueryID's
// JSON methods are pure functions of their input and share no state.

func TestParseSQLCommenterStandardTrailingComment(t *testing.T) {
	sql := "SELECT * FROM polls_question " +
		"/*controller='index',framework='django%3A4.2',route='%5Epolls/%24'*/"
	got := ParseSQLCommenter(sql)
	require.Equal(t, map[string]string{
		"controller": "index", "framework": "django:4.2", "route": "^polls/$",
	}, got)
}

func TestParseSQLCommenterLeadingAndNormalizedText(t *testing.T) {
	lead := ParseSQLCommenter("/*route='%2Fcart',controller='cart'*/ SELECT pg_sleep(2)")
	require.Equal(t, map[string]string{"route": "/cart", "controller": "cart"}, lead)
	// pg_stat_statements text: constants replaced by $n, comment kept.
	pgss := ParseSQLCommenter("SELECT count(*) FROM orders WHERE status = $1 " +
		"/*controller='checkout',db.driver='pgx',trace-id='abc'*/")
	require.Equal(t, map[string]string{
		"controller": "checkout", "db.driver": "pgx", "trace-id": "abc",
	}, pgss)
}

// Several sqlcommenter comments merge; a key repeated later wins.
func TestParseSQLCommenterMergesCommentsLaterWins(t *testing.T) {
	got := ParseSQLCommenter("/*a='1',b='2'*/ SELECT 1 /*b='3',c='4'*/")
	require.Equal(t, map[string]string{"a": "1", "b": "3", "c": "4"}, got)
}

func TestParseSQLCommenterEscapedQuote(t *testing.T) {
	got := ParseSQLCommenter(`SELECT 1 /*name='O\'Brien',team='core'*/`)
	require.Equal(t, map[string]string{"name": "O'Brien", "team": "core"}, got)
	encoded := ParseSQLCommenter("SELECT 1 /*name='O%27Brien'*/")
	require.Equal(t, map[string]string{"name": "O'Brien"}, encoded)
}

// A comment that is not entirely key='value' pairs is not a sqlcommenter
// comment and contributes nothing.
func TestParseSQLCommenterMalformedIsEmpty(t *testing.T) {
	cases := map[string]string{
		"no quotes":            "SELECT 1 /*controller=index*/",
		"unterminated value":   "SELECT 1 /*controller='index*/",
		"unterminated comment": "SELECT 1 /*controller='index'",
		"empty comment":        "SELECT 1 /**/",
		"prose comment":        "SELECT 1 /* just a note */",
		"empty key":            "SELECT 1 /*='x'*/",
		"missing comma":        "SELECT 1 /*controller='index' framework='x'*/",
		"no equals":            "SELECT 1 /*'index'*/",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			got := ParseSQLCommenter(sql)
			require.NotNil(t, got, "result must be a non-nil map")
			require.Len(t, got, 0, "malformed comment parsed: %v", got)
		})
	}
}

// Comment-looking text inside a string literal or after a -- line comment
// is not a block comment: an application user who controls a literal must
// not be able to spoof attribution tags.
func TestParseSQLCommenterIgnoresLiteralsAndLineComments(t *testing.T) {
	cases := map[string]string{
		"line comment":   "SELECT 1 -- /*controller='spoof'*/",
		"line only":      "SELECT 1 -- controller='spoof'",
		"string literal": "SELECT '/*controller=''spoof''*/' AS label",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			got := ParseSQLCommenter(sql)
			require.NotNil(t, got)
			require.Len(t, got, 0, "tags parsed from non-comment text: %v", got)
		})
	}
	mixed := ParseSQLCommenter("SELECT 'x' -- note\n/*controller='real'*/")
	require.Equal(t, map[string]string{"controller": "real"}, mixed)
}

func TestParseSQLCommenterKeyValidation(t *testing.T) {
	key64 := strings.Repeat("k", 64)
	key65 := strings.Repeat("k", 65)
	sql := "SELECT 1 /*Controller='upper',ok='y'," + key64 + "='at-limit'," +
		key65 + "='over-limit',bad%20key='enc',app_name.v-2='z'*/"
	got := ParseSQLCommenter(sql)
	require.Equal(t, map[string]string{
		"ok": "y", key64: "at-limit", "app_name.v-2": "z",
	}, got)
}

func TestParseSQLCommenterValueCap(t *testing.T) {
	at := strings.Repeat("a", 256)
	over := strings.Repeat("b", 300)
	got := ParseSQLCommenter("SELECT 1 /*at='" + at + "',over='" + over + "'*/")
	require.Equal(t, at, got["at"], "a 256-byte value is kept whole")
	require.Equal(t, strings.Repeat("b", 256), got["over"], "values cap at 256 bytes")
	// The cap never splits a UTF-8 sequence: 255 bytes + a 2-byte rune.
	multi := strings.Repeat("c", 255) + "é"
	cut := ParseSQLCommenter("SELECT 1 /*m='" + multi + "'*/")["m"]
	require.True(t, utf8.ValidString(cut), "capped value is not valid UTF-8: %q", cut)
	require.LessOrEqual(t, len(cut), 256)
	require.Equal(t, strings.Repeat("c", 255), cut)
}

func TestParseSQLCommenterNoCommentIsEmptyNonNil(t *testing.T) {
	for _, sql := range []string{"", "   ", "SELECT 1", "SELECT $1::text"} {
		got := ParseSQLCommenter(sql)
		require.NotNil(t, got, "nil map for %q", sql)
		require.Len(t, got, 0, "tags from %q: %v", sql, got)
	}
}

type queryIDHolder struct {
	ID QueryID `json:"id"`
}

func TestQueryIDUnmarshalNumberAndString(t *testing.T) {
	cases := map[string]QueryID{
		`{"id":123}`:                    123,
		`{"id":-5}`:                     -5,
		`{"id":"42"}`:                   42,
		`{"id":"-8765432109876543210"}`: QueryID(-8765432109876543210),
		// Beyond float64's 53-bit mantissa: must not round through float64.
		`{"id":8765432109876543210}`:   QueryID(8765432109876543210),
		`{"id":-9223372036854775808}`:  QueryID(-9223372036854775808),
		`{"id":"9223372036854775807"}`: QueryID(9223372036854775807),
	}
	for raw, want := range cases {
		var h queryIDHolder
		require.NoError(t, json.Unmarshal([]byte(raw), &h), raw)
		require.Equal(t, want, h.ID, raw)
	}
}

func TestQueryIDUnmarshalRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		`{"id":"abc"}`, `{"id":"12x"}`, `{"id":""}`, `{"id":"1.5"}`, `{"id":1.5}`,
		`{"id":"99999999999999999999"}`, `{"id":99999999999999999999}`,
		`{"id":true}`, `{"id":{}}`, `{"id":[1]}`, `{"id":"1e3"}`,
	} {
		var h queryIDHolder
		err := json.Unmarshal([]byte(raw), &h)
		require.Error(t, err, "accepted %s as %d", raw, h.ID)
	}
}

func TestQueryIDMarshalsAsString(t *testing.T) {
	out, err := json.Marshal(QueryID(-8765432109876543210))
	require.NoError(t, err)
	require.Equal(t, `"-8765432109876543210"`, string(out))
	out, err = json.Marshal(queryIDHolder{ID: 7})
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"7"}`, string(out))
	// TopQuery exposes it under "queryid", as a string.
	out, err = json.Marshal(TopQuery{QueryID: 8765432109876543210})
	require.NoError(t, err)
	require.Contains(t, string(out), `"queryid":"8765432109876543210"`)
	// Round trip keeps every bit.
	var back queryIDHolder
	require.NoError(t, json.Unmarshal([]byte(`{"id":"-8765432109876543210"}`), &back))
	again, err := json.Marshal(back)
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"-8765432109876543210"}`, string(again))
}
