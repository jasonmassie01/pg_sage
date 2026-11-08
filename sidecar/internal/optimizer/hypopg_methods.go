package optimizer

import "fmt"

// hypopgMethods are the index access methods HypoPG can create
// hypothetically. hypopg_create_index fails on any other (GIN, GiST,
// SP-GiST, HNSW, IVFFlat), which would read as a failed what-if.
var hypopgMethods = map[string]bool{"btree": true, "brin": true, "hash": true,
	"bloom": true}

// vectorMethods are pgvector's access methods, which pg_sage vector-lab
// measures on a copy of the workload instead.
var vectorMethods = map[string]bool{"hnsw": true, "ivfflat": true}

// HypoPGCannotModel returns why HypoPG cannot measure ddl's index, or ""
// when it can (or ddl does not parse: the what-if then reports its own
// failure). Such an index stays unverified, so an operator approves it.
func HypoPGCannotModel(ddl string) string {
	spec, err := ParseIndexDDL(ddl)
	if err != nil || hypopgMethods[spec.Method] {
		return ""
	}
	reason := fmt.Sprintf("HypoPG cannot model %s indexes; operator approval required",
		spec.Method)
	if vectorMethods[spec.Method] {
		reason += "; measure it with pg_sage vector-lab"
	}
	return reason
}
