package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

type Classification string

const (
	ClassificationAddUnique  Classification = "add_unique"
	ClassificationSetNotNull Classification = "set_not_null"
)

type StepKind string

const (
	StepCreateUniqueIndex      StepKind = "create_unique_index"
	StepAttachUniqueConstraint StepKind = "attach_unique_constraint"
	StepAddCheckNotValid       StepKind = "add_check_not_valid"
	StepValidateConstraint     StepKind = "validate_constraint"
	StepSetNotNull             StepKind = "set_not_null"
	StepDropProofConstraint    StepKind = "drop_proof_constraint"
)

type Phase string

const (
	PhaseExpand         Phase = "expand"
	PhaseContract       Phase = "contract"
	PhaseExpandComplete Phase = "expand_complete"
	PhaseExpandVerified Phase = "expand_verified"
)

type TableFacts struct {
	Schema, Name  string
	EstimatedRows int64
}
type ProofKind string

const ProofValidatedNotNullCheck ProofKind = "validated_not_null_check"

type Proof struct {
	Kind               ProofKind
	Column, Constraint string
	Validated          bool
}
type Request struct {
	SQL    string
	Cycle  int
	Table  TableFacts
	Proofs []Proof
}
type Step struct {
	Kind             StepKind
	Phase            Phase
	SQL              string
	RequiresTopLevel bool
	Destructive      bool
	// LockLevel is the table lock the step takes (migration-06); StepLock
	// folds in what the DDL classifier reports.
	LockLevel string
}
type Plan struct {
	Cycle                  int
	Classification         Classification
	Rewritten              bool
	RequiresRehearsal      bool
	ExpandSteps            []Step
	ContractSteps          []Step
	ContractNotBeforeCycle int
	// ContractNotBefore is how long after the expand phase the contract
	// phase may run (migration.contract_delay); the recorder stores the
	// wall-clock time.
	ContractNotBefore time.Duration
	// ContractGuard is the proof the contract script asserts first.
	ContractGuard ContractGuard
	// Table is the migrated table (Request.Table): the rehearsal measures
	// the statements that reference it (lab-04b). Not part of the plan's
	// JSON.
	Table TableFacts `json:"-"`
}

func (p Plan) StepsForCycle(cycle int, phase Phase) []Step {
	if phase == PhaseExpandComplete {
		return append([]Step(nil), p.ExpandSteps...)
	}
	if phase == PhaseExpandVerified && cycle >= p.ContractNotBeforeCycle {
		return append([]Step(nil), p.ContractSteps...)
	}
	return nil
}

type Planner struct{ contractDelay time.Duration }

func NewPlanner() *Planner { return &Planner{contractDelay: DefaultContractDelay} }

func (p *Planner) Plan(ctx context.Context, request Request) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, fmt.Errorf("plan migration: %w", err)
	}
	if err := validateStatement(request); err != nil {
		return Plan{}, err
	}
	stmt, err := sqlast.ParsePlannableAlter(request.SQL)
	if errors.Is(err, sqlast.ErrUnavailable) {
		return Plan{}, fmt.Errorf("online migration planning needs the parse-tree "+
			"build: %w", err)
	}
	if err != nil {
		return Plan{}, err
	}
	if err := checkTarget(stmt, request.Table); err != nil {
		return Plan{}, err
	}
	planned, err := planNotNull(request, stmt.Column)
	if stmt.Kind == sqlast.AlterAddUnique {
		planned, err = planUnique(request, stmt)
	}
	if err != nil {
		return Plan{}, err
	}
	planned.ContractNotBefore = p.delay()
	planned.Table = request.Table
	return planned, nil
}

func planUnique(request Request, stmt sqlast.PlannableAlter) (Plan, error) {
	index := stmt.Constraint + "_idx"
	if err := checkNames(append([]string{stmt.Constraint, index},
		stmt.Columns...)...); err != nil {
		return Plan{}, err
	}
	table := qualified(request.Table)
	columnSQL := quotedColumns(stmt.Columns)
	return Plan{Cycle: request.Cycle, Classification: ClassificationAddUnique,
		Rewritten: true, RequiresRehearsal: true,
		ExpandSteps: []Step{{Kind: StepCreateUniqueIndex, Phase: PhaseExpand,
			SQL: fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY %s ON %s (%s)",
				quote(index), table, columnSQL), RequiresTopLevel: true,
			LockLevel: lockShareUpdateExclusive}},
		ContractSteps: []Step{{Kind: StepAttachUniqueConstraint, Phase: PhaseContract,
			SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE USING INDEX %s%s",
				table, quote(stmt.Constraint), quote(index), stmt.Timing),
			LockLevel: lockAccessExclusive}},
		ContractNotBeforeCycle: request.Cycle + 1,
		ContractGuard: ContractGuard{Kind: GuardValidUniqueIndex,
			Schema: request.Table.Schema, Table: request.Table.Name, Name: index}}, nil
}

func planNotNull(request Request, column string) (Plan, error) {
	proofName := request.Table.Name + "_" + column + "_nn"
	if err := checkNames(column, proofName); err != nil {
		return Plan{}, err
	}
	table := qualified(request.Table)
	result := Plan{Cycle: request.Cycle, Classification: ClassificationSetNotNull,
		RequiresRehearsal: true, ContractNotBeforeCycle: request.Cycle + 1,
		ContractGuard: ContractGuard{Kind: GuardValidatedCheck,
			Schema: request.Table.Schema, Table: request.Table.Name, Name: proofName}}
	setNotNull := Step{Kind: StepSetNotNull, Phase: PhaseContract,
		SQL:       fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, quote(column)),
		LockLevel: lockAccessExclusive}
	if proof, ok := findProof(request.Proofs, column); ok {
		result.ContractGuard.Name = proof.Constraint
		result.ContractSteps = []Step{setNotNull}
		return result, nil
	}
	result.Rewritten = true
	result.ExpandSteps = []Step{
		{Kind: StepAddCheckNotValid, Phase: PhaseExpand, LockLevel: lockAccessExclusive,
			SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID",
				table, quote(proofName), quote(column))},
		{Kind: StepValidateConstraint, Phase: PhaseExpand, LockLevel: lockShareUpdateExclusive,
			SQL: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", table, quote(proofName))},
	}
	result.ContractSteps = append(result.ContractSteps, setNotNull,
		Step{Kind: StepDropProofConstraint, Phase: PhaseContract, Destructive: true,
			LockLevel: lockAccessExclusive,
			SQL:       fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", table, quote(proofName))})
	return result, nil
}

// findProof matches the folded column exactly: Proof.Column is a
// catalog attname, so "Email" proves nothing about email.
func findProof(proofs []Proof, column string) (Proof, bool) {
	for _, proof := range proofs {
		if proof.Kind == ProofValidatedNotNullCheck && proof.Validated &&
			proof.Column == column {
			return proof, true
		}
	}
	return Proof{}, false
}
func qualified(table TableFacts) string { return quote(table.Schema) + "." + quote(table.Name) }

// quote emits name, already folded, as one quoted identifier.
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func quotedColumns(columns []string) string {
	result := make([]string, 0, len(columns))
	for _, column := range columns {
		result = append(result, quote(column))
	}
	return strings.Join(result, ", ")
}
