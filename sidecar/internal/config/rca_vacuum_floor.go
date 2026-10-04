package config

import "fmt"

// Dogfood round 2: the RCA vacuum_blocked signal ("autovacuum falling
// behind") counts a table's dead-tuple ratio only above a size floor.
// Autovacuum itself ignores a table until 50 dead rows + 20% of it, and
// lifeos escalated a critical incident on tables of 57 and 6 rows.
const (
	DefaultRCAVacuumMinDeadTuples = 1000
	DefaultRCAVacuumMinTableMB    = 8
	maxRCAVacuumMinDeadTuples     = 1_000_000_000
	maxRCAVacuumMinTableMB        = 1 << 20 // 1 TB
)

// VacuumDeadTupleFloor is the fewest dead tuples a table needs for its
// ratio to count. A config built without the key (zero) gets the default,
// never "every table counts".
func (r *RCAConfig) VacuumDeadTupleFloor() int64 {
	if r.VacuumMinDeadTuples <= 0 {
		return DefaultRCAVacuumMinDeadTuples
	}
	return int64(r.VacuumMinDeadTuples)
}

// VacuumTableBytesFloor is the smallest heap, in bytes, a table needs for
// its ratio to count (zero key: the default).
func (r *RCAConfig) VacuumTableBytesFloor() int64 {
	mb := r.VacuumMinTableMB
	if mb <= 0 {
		mb = DefaultRCAVacuumMinTableMB
	}
	return int64(mb) << 20
}

func (r *RCAConfig) validateVacuumFloor() error {
	if n := r.VacuumMinDeadTuples; n < 1 || n > maxRCAVacuumMinDeadTuples {
		return fmt.Errorf("rca.vacuum_min_dead_tuples must be 1-%d, got %d",
			maxRCAVacuumMinDeadTuples, n)
	}
	if mb := r.VacuumMinTableMB; mb < 1 || mb > maxRCAVacuumMinTableMB {
		return fmt.Errorf("rca.vacuum_min_table_mb must be 1-%d, got %d",
			maxRCAVacuumMinTableMB, mb)
	}
	return nil
}
