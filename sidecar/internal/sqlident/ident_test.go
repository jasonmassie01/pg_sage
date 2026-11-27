package sqlident

import (
	"sync"
	"testing"
)

// Ident leaves a name bare only when PostgreSQL reads it back unchanged in
// every grammar position: plain lower case and not a reserved (R),
// type/function-name (T) or column-name (C) key word.
func TestIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		// Key words that broke pasted SQL: reserved ones (R).
		{"to", `"to"`}, {"desc", `"desc"`}, {"both", `"both"`}, {"only", `"only"`},
		{"window", `"window"`}, {"constraint", `"constraint"`}, {"unique", `"unique"`},
		{"user", `"user"`}, {"all", `"all"`}, {"order", `"order"`},
		// Type/function-name (T) and column-name (C) key words.
		{"left", `"left"`}, {"int", `"int"`}, {"values", `"values"`},
		{"time", `"time"`},
		// Plain names, including unreserved key words, stay readable.
		{"orders", "orders"}, {"user_id", "user_id"}, {"snake_case_1", "snake_case_1"},
		{"_agent", "_agent"}, {"agent2", "agent2"}, {"name", "name"}, {"index", "index"},
		// Anything PostgreSQL would fold or reject is quoted.
		{"Orders", `"Orders"`}, {"my table", `"my table"`}, {`we"ird`, `"we""ird"`},
		{`Sage "Agent"`, `"Sage ""Agent"""`}, {"1abc", `"1abc"`}, {"", `""`},
		{"agent-x", `"agent-x"`}, {"a$b", `"a$b"`}, {"café", `"café"`},
	}
	for _, c := range cases {
		if got := Ident(c.in); got != c.want {
			t.Errorf("Ident(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestQualified(t *testing.T) {
	cases := []struct{ schema, name, want string }{
		{"public", "orders", "public.orders"},
		{"App", "t", `"App".t`},
		{"s", "to", `s."to"`},
		{"user", "Order Items", `"user"."Order Items"`},
		{"", "", `"".""`},
	}
	for _, c := range cases {
		if got := Qualified(c.schema, c.name); got != c.want {
			t.Errorf("Qualified(%q, %q) = %s, want %s", c.schema, c.name, got, c.want)
		}
	}
}

// Role names follow the identifier rules, so a GRANT names exactly the
// role: a key-word role must be quoted or the statement does not parse.
func TestRole(t *testing.T) {
	cases := map[string]string{
		"sage_agent": "sage_agent", "agent2": "agent2", "_agent": "_agent",
		"user": `"user"`, "all": `"all"`, "SageAgent": `"SageAgent"`,
		"sage agent": `"sage agent"`, "": `""`,
	}
	for in, want := range cases {
		if got := Role(in); got != want {
			t.Errorf("Role(%q) = %s, want %s", in, got, want)
		}
	}
}

// The generated table holds only the categories that need quoting, and
// knows each word's category.
func TestKeywordTable(t *testing.T) {
	if len(keywords) < 150 {
		t.Fatalf("keyword table has %d entries; regenerate it from pg_get_keywords()",
			len(keywords))
	}
	for word, cat := range keywords {
		if cat != 'R' && cat != 'T' && cat != 'C' {
			t.Fatalf("keyword %q has category %q; only R, T and C belong here", word, cat)
		}
	}
	for word, want := range map[string]byte{"select": 'R', "left": 'T', "int": 'C'} {
		if got := keywords[word]; got != want {
			t.Errorf("keywords[%q] = %q, want %q", word, got, want)
		}
	}
	if _, ok := keywords["index"]; ok {
		t.Fatal("unreserved key word index is in the table; it never needs quoting")
	}
}

// Ident only reads the shared table, so parallel callers agree.
func TestIdentConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if Ident("to") != `"to"` || Ident("orders") != "orders" {
				errs <- "inconsistent result under concurrency"
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
}
