//go:build integration

package plan

import "testing"

// A NOT VALID referencing key is read as not validated and refused: the
// swap would re-add it enforced and VALIDATE would fail on the orphan.
func TestColumnSwapLiveRefusesNotValidReferencingFK(t *testing.T) {
	pool := planDBPool(t)
	swapSchema(t, pool,
		`CREATE TABLE swap09b.orders (id int PRIMARY KEY)`,
		`CREATE TABLE swap09b.lines (id int PRIMARY KEY, order_id int)`,
		`INSERT INTO swap09b.lines VALUES (1, 42)`,
		`ALTER TABLE swap09b.lines ADD CONSTRAINT lines_order_fk
			FOREIGN KEY (order_id) REFERENCES swap09b.orders (id) NOT VALID`)
	facts, err := readFacts(t, pool, "orders", "id")
	if err != nil {
		t.Fatalf("ReadSwapFacts: %v", err)
	}
	if len(facts.Referencing) != 1 || !facts.Referencing[0].NotValidated {
		t.Fatalf("referencing %+v, want one key read as not validated", facts.Referencing)
	}
	refuse(t, facts, SwapRequest{Schema: "swap09b", Table: "orders", Column: "id",
		TargetType: "bigint"}, "lines_order_fk is not validated")
}

// The primary key's shape is read from the catalog: a deferrable key, a
// key with INCLUDE columns and a replica-identity key are each refused,
// and a plain key reads none of the three.
func TestColumnSwapLiveReadsPrimaryKeyShape(t *testing.T) {
	pool := planDBPool(t)
	cases := map[string][]string{
		"deferrable": {`CREATE TABLE swap09b.t (id int PRIMARY KEY DEFERRABLE, a text)`},
		"INCLUDE columns": {`CREATE TABLE swap09b.t (id int, a text,
			CONSTRAINT t_pkey PRIMARY KEY (id) INCLUDE (a))`},
		"replica identity": {`CREATE TABLE swap09b.t (id int PRIMARY KEY, a text)`,
			`ALTER TABLE swap09b.t REPLICA IDENTITY USING INDEX t_pkey`},
	}
	for name, ddl := range cases {
		swapSchema(t, pool, ddl...)
		facts, err := readFacts(t, pool, "t", "id")
		if err != nil {
			t.Fatalf("%s: ReadSwapFacts: %v", name, err)
		}
		refuse(t, facts, SwapRequest{Schema: "swap09b", Table: "t", Column: "id",
			TargetType: "bigint"}, name)
	}
	swapSchema(t, pool, `CREATE TABLE swap09b.t (id int PRIMARY KEY, a text)`)
	facts, err := readFacts(t, pool, "t", "id")
	if err != nil {
		t.Fatalf("plain key: ReadSwapFacts: %v", err)
	}
	k := facts.PrimaryKey
	if k == nil || k.Deferrable || k.Include || k.ReplicaIdentity {
		t.Fatalf("plain primary key read as %+v", k)
	}
}
