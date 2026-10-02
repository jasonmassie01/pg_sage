package runbook

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A runbook definition decodes strictly (unknown fields, trailing data
// and oversized documents are refused) and hashes canonically: the hash
// is the identity a signature binds, so it ignores key order and
// whitespace and changes with any field.

func TestDecode_RoundTripsTheTypedDefinition(t *testing.T) {
	got, err := Decode([]byte(lockRunbookJSON))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(got, lockRunbook()) {
		t.Fatalf("decoded = %+v\nwant %+v", got, lockRunbook())
	}
	if problems := Validate(got, testVocab); problems != nil {
		t.Fatalf("valid runbook has problems: %v", problems)
	}
}

func TestDecode_RefusesAmbiguousDocuments(t *testing.T) {
	cases := map[string]string{
		"empty":         ``,
		"not an object": `[1, 2]`,
		"unknown field": strings.Replace(lockRunbookJSON, `"start"`,
			`"sql": "DROP TABLE t", "start"`, 1),
		"unknown nested field": strings.Replace(lockRunbookJSON, `"agg": "max"`,
			`"agg": "max", "query": "SELECT 1"`, 1),
		"trailing data": lockRunbookJSON + ` {"name": "again"}`,
		"oversized": `{"name": "x", "description": "` +
			strings.Repeat("a", MaxDefinitionBytes) + `"}`,
		"wrong type": strings.Replace(lockRunbookJSON, `"value": 300`,
			`"value": "300"`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode([]byte(raw))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Decode(%s) = %v, want ErrInvalid", name, err)
			}
		})
	}
}

func TestDecode_EmptyArgsNormalizeToNone(t *testing.T) {
	raw := strings.Replace(lockRunbookJSON, `"probe": "long_transactions",
     "next"`, `"probe": "long_transactions", "args": {}, "next"`, 1)
	got, err := Decode([]byte(raw))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Nodes[0].Args != nil {
		t.Fatalf("empty args kept as %+v; they hash differently from no args",
			got.Nodes[0].Args)
	}
	a, _ := got.Hash()
	b, _ := lockRunbook().Hash()
	if a != b {
		t.Fatalf("hash with {} args %s != without args %s", a, b)
	}
}

func TestHash_IgnoresKeyOrderAndWhitespace(t *testing.T) {
	reordered := `{"nodes":[{"next":"old_tx","probe":"long_transactions","type":"probe",` +
		`"id":"read_long_tx"},{"else":"escalate","then":"end_tx","when":{"value":300,` +
		`"cmp":">=","agg":"max","column":"xact_age_s","probe":"long_transactions",` +
		`"op":"column"},"type":"decision","id":"old_tx"},{"proposal":{"node":` +
		`"idle_in_tx_holder","kind":"operator_step"},"type":"proposal","id":"end_tx"},` +
		`{"proposal":{"kind":"escalate"},"type":"proposal","id":"escalate"}],` +
		`"start":"read_long_tx","trigger":{"nodes":["idle_in_tx_holder"],"kinds":` +
		`["lock_blocking"]},"description":"Confirm an idle holder and point at its ` +
		`operator step.","name":"Idle-in-transaction blocker"}`
	d, err := Decode([]byte(reordered))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	a, err := d.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, _ := lockRunbook().Hash()
	if a != b || len(a) != 64 || strings.ToLower(a) != a {
		t.Fatalf("hashes %q and %q, want equal lower-case sha256 hex", a, b)
	}
}

func TestHash_ChangesWithEveryField(t *testing.T) {
	base, _ := lockRunbook().Hash()
	mutations := map[string]func(*Definition){
		"name":        func(d *Definition) { d.Name += "!" },
		"description": func(d *Definition) { d.Description = "" },
		"trigger":     func(d *Definition) { d.Trigger.Kinds = append(d.Trigger.Kinds, "x") },
		"start":       func(d *Definition) { d.Start = "old_tx" },
		"threshold":   func(d *Definition) { d.Nodes[1].When.Value = num(301) },
		"cmp":         func(d *Definition) { d.Nodes[1].When.Cmp = ">" },
		"branch":      func(d *Definition) { d.Nodes[1].Then = "escalate" },
		"note":        func(d *Definition) { d.Nodes[0].Note = "read it" },
		"proposal":    func(d *Definition) { d.Nodes[2].Proposal.Node = "ddl_lock_queue" },
		"window": func(d *Definition) {
			d.Nodes[0].Args = &ProbeArgs{WindowSeconds: 600}
		},
	}
	seen := map[string]string{base: "base"}
	for name, mutate := range mutations {
		d := lockRunbook()
		mutate(&d)
		h, err := d.Hash()
		if err != nil {
			t.Fatalf("%s: Hash: %v", name, err)
		}
		if prev, dup := seen[h]; dup {
			t.Errorf("mutating %s gives the hash of %s", name, prev)
		}
		seen[h] = name
	}
}

func TestMatch_ByTriggerKindAndOpenNodes(t *testing.T) {
	d := lockRunbook()
	cases := []struct {
		name     string
		kind     string
		open     []string
		ok       bool
		specific int
	}{
		{"kind and node", "lock_blocking", []string{"ddl_lock_queue", "idle_in_tx_holder"},
			true, 1},
		{"node not open", "lock_blocking", []string{"hot_row_contention"}, false, 0},
		{"nothing open", "lock_blocking", nil, false, 0},
		{"other kind", "wal_retention", []string{"idle_in_tx_holder"}, false, 0},
		{"empty kind", "", []string{"idle_in_tx_holder"}, false, 0},
	}
	for _, c := range cases {
		ok, specific := d.Match(c.kind, c.open)
		if ok != c.ok || specific != c.specific {
			t.Errorf("%s: Match = (%v, %d), want (%v, %d)", c.name, ok, specific,
				c.ok, c.specific)
		}
	}
	d.Trigger.Nodes = nil
	if ok, specific := d.Match("lock_blocking", nil); !ok || specific != 0 {
		t.Fatalf("kind-only runbook: Match = (%v, %d), want (true, 0)", ok, specific)
	}
}

func TestNode_LooksUpByID(t *testing.T) {
	d := lockRunbook()
	n, ok := d.Node("old_tx")
	if !ok || n.Type != NodeDecision || n.Then != "end_tx" {
		t.Fatalf("Node(old_tx) = %+v, %v", n, ok)
	}
	if _, ok := d.Node("missing"); ok {
		t.Fatal("Node(missing) found a node")
	}
	if _, ok := (Definition{}).Node(""); ok {
		t.Fatal("empty definition found a node")
	}
}
