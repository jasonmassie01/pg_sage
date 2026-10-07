package snapstore

import (
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"regexp"
	"strconv"
	"testing"
)

// The delta encoder's integer checks run on every field of every element
// each cycle (perf gate: most of the sidecar's CPU at 20,000 relations).
// The fast paths must agree exactly with the integer grammar and with
// arbitrary-precision arithmetic.

var plainIntegerGrammar = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

func bigDiff(base, cur string) string {
	b, _ := new(big.Int).SetString(base, 10)
	c, _ := new(big.Int).SetString(cur, 10)
	return c.Sub(c, b).String()
}

func bigSum(a, b string) string {
	x, _ := new(big.Int).SetString(a, 10)
	y, _ := new(big.Int).SetString(b, 10)
	return x.Add(x, y).String()
}

func TestIsPlainIntegerMatchesGrammar(t *testing.T) {
	inputs := []string{"", "0", "-0", "00", "01", "-", "--1", "1", "-1", "123", "1e3",
		"1.0", `"1"`, " 1", "1 ", "+1", "9223372036854775807", "-9223372036854775808",
		"9223372036854775808", "99999999999999999999999", "null", "true", "-01", "0x1"}
	rng := rand.New(rand.NewSource(1))
	const alphabet = "-0123456789e.+ "
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.Intn(25))
		for j := range b {
			b[j] = alphabet[rng.Intn(len(alphabet))]
		}
		inputs = append(inputs, string(b))
	}
	for _, in := range inputs {
		if got, want := isPlainInteger([]byte(in)), plainIntegerGrammar.MatchString(in); got != want {
			t.Fatalf("isPlainInteger(%q) = %t, grammar says %t", in, got, want)
		}
	}
}

func integerPairs() [][2]string {
	pairs := [][2]string{{"0", "0"}, {"5", "5"}, {"-1", "1"}, {"1", "-1"},
		{"9223372036854775807", "-9223372036854775808"},
		{"-9223372036854775808", "9223372036854775807"},
		{"9223372036854775807", "9223372036854775807"},
		{"999999999999999999", "-999999999999999999"},
		{"99999999999999999999999", "1"}, {"1", "-99999999999999999999999"},
		{"123456789012345678", "1234567890123456789"}}
	rng := rand.New(rand.NewSource(2))
	num := func() string {
		switch rng.Intn(4) {
		case 0:
			return strconv.FormatInt(rng.Int63n(1000)-500, 10)
		case 1:
			return strconv.FormatInt(rng.Int63()-rng.Int63(), 10)
		case 2:
			digits := 1 + rng.Intn(24)
			s := []byte{byte('1' + rng.Intn(9))}
			for j := 1; j < digits; j++ {
				s = append(s, byte('0'+rng.Intn(10)))
			}
			if rng.Intn(2) == 0 {
				return "-" + string(s)
			}
			return string(s)
		default:
			return strconv.FormatInt(rng.Int63(), 10)
		}
	}
	for i := 0; i < 20000; i++ {
		pairs = append(pairs, [2]string{num(), num()})
	}
	return pairs
}

func TestIncrementMatchesBigIntArithmetic(t *testing.T) {
	for _, p := range integerPairs() {
		got, ok := increment(json.RawMessage(p[0]), json.RawMessage(p[1]))
		if !ok || string(got) != bigDiff(p[0], p[1]) {
			t.Fatalf("increment(%s, %s) = %s %t, want %s", p[0], p[1], got, ok,
				bigDiff(p[0], p[1]))
		}
	}
}

func TestAddMatchesBigIntArithmetic(t *testing.T) {
	for _, p := range integerPairs() {
		if got := add(json.RawMessage(p[0]), json.RawMessage(p[1])); string(got) !=
			bigSum(p[0], p[1]) {
			t.Fatalf("add(%s, %s) = %s, want %s", p[0], p[1], got, bigSum(p[0], p[1]))
		}
	}
}

// Equal values: an integer moved by zero; anything else is not an
// increment at all, however equal.
func TestIncrementOfEqualValues(t *testing.T) {
	for _, v := range []string{"0", "42", "-7", "99999999999999999999999"} {
		if got, ok := increment(json.RawMessage(v), json.RawMessage(v)); !ok ||
			string(got) != "0" {
			t.Fatalf("increment(%s, %s) = %s %t, want 0", v, v, got, ok)
		}
	}
	for _, v := range []string{`"x"`, "1.5", "1e3", "null", "true", "", "01"} {
		if _, ok := increment(json.RawMessage(v), json.RawMessage(v)); ok {
			t.Fatalf("increment of non-integer %q claimed ok", v)
		}
	}
}

// benchTables is a tables document of n elements with 30 counters, and the
// next cycle's: xid_age moved everywhere, a few counters on a few tables.
func benchTables(n int) (base, cur []byte) {
	mk := func(cycle int) []byte {
		items := make([]map[string]any, n)
		for i := range items {
			item := map[string]any{"schemaname": "app", "relname": fmt.Sprintf("t_%d", i),
				"xid_age": 1000 + cycle*7, "note": "steady"}
			for f := 0; f < 28; f++ {
				v := i*31 + f
				if f == 0 && i%50 == 0 {
					v += cycle * 3
				}
				item[fmt.Sprintf("c%02d", f)] = v
			}
			items[i] = item
		}
		b, _ := json.Marshal(items)
		return b
	}
	return mk(0), mk(1)
}

func BenchmarkEncodeDeltaTables(b *testing.B) {
	baseDoc, curDoc := benchTables(20000)
	base, err := parseCatalog(baseDoc, keyFields["tables"])
	if err != nil {
		b.Fatal(err)
	}
	cur, err := parseCatalog(curDoc, keyFields["tables"])
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := encodeDelta(base, cur); err != nil {
			b.Fatal(err)
		}
	}
}
