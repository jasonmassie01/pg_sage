package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/pg-sage/sidecar/internal/migration"
)

// DefaultContractDelay is migration.contract_delay's default: how long
// after the expand phase the contract phase may run (migration-06).
const DefaultContractDelay = time.Hour

// ContractLockTimeout bounds the contract script's lock waits; the
// migration applier uses the same value.
const ContractLockTimeout = 5 * time.Second

const lockShareUpdateExclusive = "SHARE UPDATE EXCLUSIVE"

// guardTag dollar-quotes the contract script's guard block.
const guardTag = "$pg_sage_guard$"

// GuardKind names what the expand phase proved for the contract phase.
type GuardKind string

const (
	// GuardValidatedCheck: a CHECK (col IS NOT NULL) that is convalidated.
	GuardValidatedCheck GuardKind = "validated_check"
	// GuardValidUniqueIndex: a unique index that is indisvalid.
	GuardValidUniqueIndex GuardKind = "valid_unique_index"
)

// ContractGuard is the object the contract script asserts before it runs
// a contract step: Name is a constraint on Schema.Table, or an index in
// Schema.
type ContractGuard struct {
	Kind                GuardKind
	Schema, Table, Name string
}

// WithContractDelay sets the delay plans carry; zero or negative keeps
// DefaultContractDelay (config load refuses those values).
func (p *Planner) WithContractDelay(delay time.Duration) *Planner {
	if p != nil && delay > 0 {
		p.contractDelay = delay
	}
	return p
}

func (p *Planner) delay() time.Duration {
	if p == nil || p.contractDelay <= 0 {
		return DefaultContractDelay
	}
	return p.contractDelay
}

// StepLock is the stronger of the lock the step states and the lock
// migration-04's classifier reports for its SQL. A step that states no
// lock, or whose SQL the classifier cannot read, is ACCESS EXCLUSIVE.
func StepLock(ctx context.Context, step Step) string {
	strongest := step.LockLevel
	if _, known := lockRank[strongest]; !known || strongest == "" {
		strongest = lockAccessExclusive
	}
	res, err := migration.ClassifyStatements(ctx, nil, step.SQL, 0)
	if err != nil || res.Untokenizable {
		return lockAccessExclusive
	}
	for _, c := range res.All() {
		if lockRank[c.LockLevel] > lockRank[strongest] {
			strongest = c.LockLevel
		}
	}
	return strongest
}

// ContractScript renders the contract steps as a forward-fix script an
// operator runs after contract_not_before. pg_sage never executes it.
// The script is one transaction: a guard block raises unless the expand
// phase's proof still holds, which aborts every later statement, so a
// client that ignores errors still changes nothing.
func (p Plan) ContractScript() (string, error) {
	if len(p.ContractSteps) == 0 {
		return "", errors.New("contract script: the plan has no contract steps")
	}
	guard, err := guardBlock(p.ContractGuard)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "-- pg_sage contract script for %s (forward fix; review it first).\n",
		commentText(qualified(TableFacts{Schema: p.ContractGuard.Schema,
			Name: p.ContractGuard.Table})))
	b.WriteString("-- Run it after contract_not_before. It stops, changing nothing,\n" +
		"-- unless the expand phase's proof still holds.\nBEGIN;\n")
	b.WriteString(guard)
	fmt.Fprintf(&b, "SET LOCAL lock_timeout = '%dms';\n", ContractLockTimeout.Milliseconds())
	for _, step := range p.ContractSteps {
		if step.RequiresTopLevel {
			return "", fmt.Errorf("contract script: step %s cannot run in a transaction",
				step.Kind)
		}
		fmt.Fprintf(&b, "-- step %s (lock: %s)\n%s;\n", step.Kind, step.LockLevel, step.SQL)
	}
	b.WriteString("COMMIT;\n")
	return b.String(), nil
}

// guardBlock is the DO block that raises unless guard holds.
func guardBlock(guard ContractGuard) (string, error) {
	if guard.Name == "" || guard.Schema == "" || guard.Table == "" {
		return "", errors.New("contract script: the plan names no proof to assert")
	}
	table := literal(qualified(TableFacts{Schema: guard.Schema, Name: guard.Table}))
	var condition string
	switch guard.Kind {
	case GuardValidatedCheck:
		condition = fmt.Sprintf("SELECT 1 FROM pg_catalog.pg_constraint WHERE conrelid = "+
			"pg_catalog.to_regclass(%s) AND conname = %s AND contype = 'c' AND convalidated",
			table, literal(guard.Name))
	case GuardValidUniqueIndex:
		index := literal(qualified(TableFacts{Schema: guard.Schema, Name: guard.Name}))
		condition = fmt.Sprintf("SELECT 1 FROM pg_catalog.pg_index WHERE indexrelid = "+
			"pg_catalog.to_regclass(%s) AND indrelid = pg_catalog.to_regclass(%s) "+
			"AND indisvalid AND indisunique", index, table)
	default:
		return "", fmt.Errorf("contract script: unknown proof kind %q", guard.Kind)
	}
	block := fmt.Sprintf("DO %s\nBEGIN\n  IF NOT EXISTS (%s) THEN\n"+
		"    RAISE EXCEPTION 'pg_sage: proof %% (%s) does not hold; run nothing', %s;\n"+
		"  END IF;\nEND\n%s;\n", guardTag, condition, guard.Kind, literal(guard.Name), guardTag)
	if strings.Count(block, guardTag) != 2 {
		return "", errors.New("contract script: a name contains the guard's quote tag")
	}
	return block, nil
}

// literal quotes s as one SQL string literal. A value with a backslash
// becomes an escape-string (E) literal with the backslash doubled, so it reads the
// same whether or not the session has standard_conforming_strings on.
func literal(s string) string {
	quoted := strings.ReplaceAll(s, "'", "''")
	if !strings.Contains(s, `\`) {
		return "'" + quoted + "'"
	}
	return "E'" + strings.ReplaceAll(quoted, `\`, `\\`) + "'"
}

// commentText makes s safe inside a "--" comment: a control character
// (a newline above all) would end the comment and turn the rest of a
// name into SQL the operator runs.
func commentText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}
