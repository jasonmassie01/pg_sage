package plan

import (
	"context"
	"fmt"
	"regexp"
	"strings"
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
}
type Plan struct {
	Cycle                  int
	Classification         Classification
	Rewritten              bool
	RequiresRehearsal      bool
	ExpandSteps            []Step
	ContractSteps          []Step
	ContractNotBeforeCycle int
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

type Planner struct{}

func NewPlanner() *Planner { return &Planner{} }

var addUniquePattern = regexp.MustCompile(
	`(?i)ADD\s+CONSTRAINT\s+([a-zA-Z0-9_]+)\s+UNIQUE\s*\(([^)]+)\)`,
)

var setNotNullPattern = regexp.MustCompile(
	`(?i)ALTER\s+COLUMN\s+([a-zA-Z0-9_]+)\s+SET\s+NOT\s+NULL`,
)

func (p *Planner) Plan(ctx context.Context, request Request) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, fmt.Errorf("plan migration: %w", err)
	}
	if match := addUniquePattern.FindStringSubmatch(request.SQL); len(match) == 3 {
		return planUnique(request, match[1], splitColumns(match[2])), nil
	}
	if match := setNotNullPattern.FindStringSubmatch(request.SQL); len(match) == 2 {
		return planNotNull(request, match[1]), nil
	}
	return Plan{}, fmt.Errorf("unsupported migration statement")
}

func planUnique(request Request, constraint string, columns []string) Plan {
	table := qualified(request.Table)
	index := constraint + "_idx"
	columnSQL := quotedColumns(columns)
	return Plan{Cycle: request.Cycle, Classification: ClassificationAddUnique,
		Rewritten: true, RequiresRehearsal: true,
		ExpandSteps: []Step{{Kind: StepCreateUniqueIndex, Phase: PhaseExpand,
			SQL: fmt.Sprintf("CREATE UNIQUE INDEX CONCURRENTLY %s ON %s (%s)",
				quote(index), table, columnSQL), RequiresTopLevel: true}},
		ContractSteps: []Step{{Kind: StepAttachUniqueConstraint, Phase: PhaseContract,
			SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE USING INDEX %s",
				table, quote(constraint), quote(index))}},
		ContractNotBeforeCycle: request.Cycle + 1}
}

func planNotNull(request Request, column string) Plan {
	table := qualified(request.Table)
	proofName := request.Table.Name + "_" + column + "_nn"
	result := Plan{Cycle: request.Cycle, Classification: ClassificationSetNotNull,
		RequiresRehearsal: true, ContractNotBeforeCycle: request.Cycle + 1}
	if !hasProof(request.Proofs, column) {
		result.Rewritten = true
		result.ExpandSteps = []Step{
			{Kind: StepAddCheckNotValid, Phase: PhaseExpand,
				SQL: fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID",
					table, quote(proofName), quote(column))},
			{Kind: StepValidateConstraint, Phase: PhaseExpand,
				SQL: fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", table, quote(proofName))},
		}
		result.ContractSteps = append(result.ContractSteps,
			Step{Kind: StepSetNotNull, Phase: PhaseContract,
				SQL: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, quote(column))},
			Step{Kind: StepDropProofConstraint, Phase: PhaseContract, Destructive: true,
				SQL: fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", table, quote(proofName))})
		return result
	}
	result.ContractSteps = []Step{{Kind: StepSetNotNull, Phase: PhaseContract,
		SQL: fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", table, quote(column))}}
	return result
}

func hasProof(proofs []Proof, column string) bool {
	for _, proof := range proofs {
		if proof.Kind == ProofValidatedNotNullCheck && proof.Validated &&
			strings.EqualFold(proof.Column, column) {
			return true
		}
	}
	return false
}
func qualified(table TableFacts) string { return quote(table.Schema) + "." + quote(table.Name) }
func quote(value string) string {
	return `"` + strings.ReplaceAll(strings.TrimSpace(value), `"`, `""`) + `"`
}
func splitColumns(raw string) []string { return strings.Split(raw, ",") }
func quotedColumns(columns []string) string {
	result := make([]string, 0, len(columns))
	for _, column := range columns {
		result = append(result, quote(strings.TrimSpace(column)))
	}
	return strings.Join(result, ", ")
}
