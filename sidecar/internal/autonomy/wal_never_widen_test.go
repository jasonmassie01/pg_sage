package autonomy

import (
	"context"
	"fmt"
	"regexp"
	"testing"
)

// clouds-07 never-widen (CHECK-03): provider capacity only tightens the
// WAL custodian. Across capacity known/unknown x slot states, every slot
// proposed for without capacity is proposed for with it, a bound keeps
// the same size, no drop ever appears (AllowDrop stays false) and every
// SQL is the cluster-wide max_slot_wal_keep_size bound the policy gate
// already admits or parks.

var boundSQLPattern = regexp.MustCompile(
	`^ALTER SYSTEM SET max_slot_wal_keep_size = '[0-9]+MB'$`)

func neverWidenSlots() []walSlot {
	var slots []walSlot
	for _, slotType := range []string{"logical", "physical"} {
		for _, active := range []bool{true, false} {
			for _, retained := range []int64{0, gib, 26 * gib, 30 * gib, 41 * gib, 70 * gib} {
				slots = append(slots, walSlot{
					name:     fmt.Sprintf("%s_%t_%d", slotType, active, retained/gib),
					slotType: slotType, active: active, retained: retained})
			}
		}
	}
	return slots
}

func TestWALProviderCapacityNeverWidens(t *testing.T) {
	ctx := context.Background()
	pool := requireAutonomyDB(t) // one pool: 96 custodians would hold 96 connections
	capacities := []int64{40 * gib, 100 * gib, 1000 * gib}
	for _, slot := range neverWidenSlots() {
		without, err := diskCustodianOn(pool, nil).proposeFor(ctx, []walSlot{slot})
		if err != nil {
			t.Fatalf("%s without capacity: %v", slot.name, err)
		}
		for _, capacity := range capacities {
			with, err := diskCustodianOn(pool, &fakeDisk{bytes: capacity}).proposeFor(ctx,
				[]walSlot{slot})
			if err != nil {
				t.Fatalf("%s with %d GiB: %v", slot.name, capacity/gib, err)
			}
			assertNotWider(t, fmt.Sprintf("%s @ %d GiB", slot.name, capacity/gib),
				without, with)
		}
	}
}

// Several slots in one scan: the one cluster-wide bound is still proposed
// at most once, and with the same size whenever both scans propose it.
func TestWALProviderCapacityNeverWidensAcrossSlots(t *testing.T) {
	ctx := context.Background()
	pool := requireAutonomyDB(t)
	slots := neverWidenSlots()
	without, err := diskCustodianOn(pool, nil).proposeFor(ctx, slots)
	if err != nil {
		t.Fatal(err)
	}
	with, err := diskCustodianOn(pool, &fakeDisk{bytes: 1000 * gib}).proposeFor(ctx, slots)
	if err != nil {
		t.Fatal(err)
	}
	wantSQL, gotSQL := boundSQLs(without), boundSQLs(with)
	if len(wantSQL) != 1 || len(gotSQL) != 1 || wantSQL[0] != gotSQL[0] {
		t.Fatalf("bounds without = %q, with = %q; want the same single bound",
			wantSQL, gotSQL)
	}
	for _, p := range with {
		assertAdmissible(t, "across slots", p)
	}
}

func assertNotWider(t *testing.T, label string, without, with []Proposal) {
	t.Helper()
	for _, p := range with {
		assertAdmissible(t, label, p)
	}
	for _, base := range without {
		match := proposalFor(with, base.TargetObjects[0][len("slot:"):])
		if match == nil {
			t.Fatalf("%s: proposal for %v lost with capacity", label, base.TargetObjects)
		}
		if base.SQL != "" && match.SQL != "" && base.SQL != match.SQL {
			t.Fatalf("%s: bound changed from %q to %q", label, base.SQL, match.SQL)
		}
		if base.SQL == "" && match.SQL != "" {
			t.Fatalf("%s: plan-only escalation became SQL %q", label, match.SQL)
		}
	}
}

func assertAdmissible(t *testing.T, label string, p Proposal) {
	t.Helper()
	if p.Feature != "wal" || p.ResponseKind != "" || p.Deadline != nil {
		t.Fatalf("%s: proposal shape changed: %#v", label, p)
	}
	if p.SQL != "" && !boundSQLPattern.MatchString(p.SQL) {
		t.Fatalf("%s: SQL %q is not the max_slot_wal_keep_size bound", label, p.SQL)
	}
	if p.SQL == "" && p.Plan == "" {
		t.Fatalf("%s: empty proposal returned", label)
	}
}

func boundSQLs(ps []Proposal) []string {
	var out []string
	for _, p := range ps {
		if p.SQL != "" {
			out = append(out, p.SQL)
		}
	}
	return out
}
