package agentdb

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type DryRunProvisionRunner struct{}

func (DryRunProvisionRunner) Run(
	_ context.Context,
	command ProviderCommand,
) ProvisionRunResult {
	joined := strings.Join(command.Args, " ")
	return ProvisionRunResult{
		ExitCode: 0,
		Stdout:   "dry run: " + joined,
		Detail: map[string]any{
			"tool": command.Tool,
			"mode": "dry_run",
		},
	}
}

func (s *Store) PreflightProvision(
	ctx context.Context,
	id string,
) (ProvisionAttempt, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	commands, err := commandsFromPlan(dep.ProvisioningPlan)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	attempt, err := s.recordProvisionAttempt(ctx, dep.DeploymentID, provisionAttemptInput{
		Kind:    "preflight",
		Status:  "passed",
		Runner:  "validator",
		Command: commandArgs(commands),
		Stdout:  "validated cloud instance plan for dry-run execution",
		Detail: map[string]any{
			"provider":      dep.Provider,
			"command_count": len(commands),
		},
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.updateProvisioningStatus(ctx, dep.DeploymentID, "preflight_passed", nil); err != nil {
		return ProvisionAttempt{}, err
	}
	_ = s.audit(ctx, dep.DeploymentID, "provision_preflight", attempt.Detail)
	return attempt, nil
}

func (s *Store) ExecuteProvision(
	ctx context.Context,
	id string,
	runner ProvisionRunner,
) (ProvisionAttempt, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if dep.ProvisioningStatus == "dry_run_ready" || dep.ProvisioningStatus == "ready" {
		return ProvisionAttempt{}, ErrConflict
	}
	if dep.ProvisioningStatus != "preflight_passed" && dep.ProvisioningStatus != "failed" {
		return ProvisionAttempt{}, ErrInvalid
	}
	if runner == nil {
		runner = DryRunProvisionRunner{}
	}
	commands, err := commandsFromPlan(dep.ProvisioningPlan)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.updateProvisioningStatus(ctx, dep.DeploymentID, "provisioning", nil); err != nil {
		return ProvisionAttempt{}, err
	}
	result := runner.Run(ctx, commands[0])
	status := "succeeded"
	nextStatus := "dry_run_ready"
	if result.ExitCode != 0 {
		status = "failed"
		nextStatus = "failed"
	}
	attempt, err := s.recordProvisionAttempt(ctx, dep.DeploymentID, provisionAttemptInput{
		Kind:       "execute",
		Status:     status,
		Runner:     "dry_run",
		Command:    commands[0].Args,
		ExitCode:   result.ExitCode,
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		Detail:     result.Detail,
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return ProvisionAttempt{}, err
	}
	conn := map[string]any{"execution_mode": "dry_run"}
	if err := s.updateProvisioningStatus(ctx, dep.DeploymentID, nextStatus, conn); err != nil {
		return ProvisionAttempt{}, err
	}
	_ = s.audit(ctx, dep.DeploymentID, "provision_execute_"+status, attempt.Detail)
	if result.ExitCode != 0 {
		return attempt, ErrInvalid
	}
	return attempt, nil
}

func (s *Store) CheckProvisionStatus(
	ctx context.Context,
	id string,
	runner ProvisionRunner,
) (ProvisionAttempt, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	command, err := providerLifecycleCommand(dep, "status")
	if err != nil {
		return ProvisionAttempt{}, err
	}
	return s.runLifecycleCommand(ctx, dep, lifecycleRunInput{
		Kind:       "status_check",
		Command:    command,
		Runner:     runner,
		NextStatus: "status_checked",
		Failure:    "failed",
	})
}

func (s *Store) DestroyProvisionDryRun(
	ctx context.Context,
	id string,
	runner ProvisionRunner,
) (ProvisionAttempt, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.requireRestoreVerifiedBackup(ctx, dep); err != nil {
		return ProvisionAttempt{}, err
	}
	command, err := providerLifecycleCommand(dep, "destroy")
	if err != nil {
		return ProvisionAttempt{}, err
	}
	return s.runLifecycleCommand(ctx, dep, lifecycleRunInput{
		Kind:       "destroy_dry_run",
		Command:    command,
		Runner:     runner,
		NextStatus: "destroy_dry_run_ready",
		Failure:    "failed",
	})
}

func (s *Store) ProvisionAttempts(
	ctx context.Context,
	id string,
) ([]ProvisionAttempt, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT attempt_id, deployment_id, kind, status, runner, command,
			exit_code, stdout, stderr, detail, created_at, finished_at
		FROM sage.agent_db_provision_attempts
		WHERE deployment_id=$1
		ORDER BY created_at ASC, attempt_id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProvisionAttempt{}
	for rows.Next() {
		var attempt ProvisionAttempt
		if err := scanProvisionAttempt(rows, &attempt); err != nil {
			return nil, err
		}
		out = append(out, attempt)
	}
	return out, rows.Err()
}

type lifecycleRunInput struct {
	Kind       string
	Command    ProviderCommand
	Runner     ProvisionRunner
	NextStatus string
	Failure    string
}

func (s *Store) runLifecycleCommand(
	ctx context.Context,
	dep Deployment,
	input lifecycleRunInput,
) (ProvisionAttempt, error) {
	runner := input.Runner
	if runner == nil {
		runner = DryRunProvisionRunner{}
	}
	result := runner.Run(ctx, input.Command)
	status := "succeeded"
	nextStatus := input.NextStatus
	if result.ExitCode != 0 {
		status = "failed"
		nextStatus = input.Failure
	}
	attempt, err := s.recordProvisionAttempt(ctx, dep.DeploymentID, provisionAttemptInput{
		Kind:       input.Kind,
		Status:     status,
		Runner:     "dry_run",
		Command:    input.Command.Args,
		ExitCode:   result.ExitCode,
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		Detail:     result.Detail,
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.updateProvisioningStatus(ctx, dep.DeploymentID, nextStatus, nil); err != nil {
		return ProvisionAttempt{}, err
	}
	_ = s.audit(ctx, dep.DeploymentID, input.Kind+"_"+status, attempt.Detail)
	if result.ExitCode != 0 {
		return attempt, ErrInvalid
	}
	return attempt, nil
}

func (s *Store) requireRestoreVerifiedBackup(
	ctx context.Context,
	dep Deployment,
) error {
	if !dep.BackupRequired {
		return nil
	}
	backups, err := s.Backups(ctx, dep.DeploymentID)
	if err != nil {
		return err
	}
	if !hasRestoreVerifiedBackup(backups) {
		return ErrRestoreRequired
	}
	return nil
}

func (s *Store) cloudDeploymentForExecution(
	ctx context.Context,
	id string,
) (Deployment, error) {
	dep, err := s.Get(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if dep.Provider == ProviderLocalPostgres || dep.ProvisioningLevel != LevelInstance {
		return Deployment{}, ErrInvalid
	}
	if _, err := commandsFromPlan(dep.ProvisioningPlan); err != nil {
		return Deployment{}, err
	}
	return dep, nil
}

func (s *Store) updateProvisioningStatus(
	ctx context.Context,
	id string,
	status string,
	connectionInfo map[string]any,
) error {
	if connectionInfo == nil {
		connectionInfo = map[string]any{}
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET provisioning_status=$2,
			connection_info=connection_info || $3::jsonb,
			updated_at=now()
		WHERE deployment_id=$1`, id, status, jsonBytes(connectionInfo))
	return err
}

type provisionAttemptInput struct {
	Kind       string
	Status     string
	Runner     string
	Command    []string
	ExitCode   int
	Stdout     string
	Stderr     string
	Detail     map[string]any
	FinishedAt time.Time
}

func (s *Store) recordProvisionAttempt(
	ctx context.Context,
	id string,
	input provisionAttemptInput,
) (ProvisionAttempt, error) {
	var finishedAt *time.Time
	if !input.FinishedAt.IsZero() {
		finishedAt = &input.FinishedAt
	}
	var attempt ProvisionAttempt
	err := scanProvisionAttempt(s.pool.QueryRow(ctx, `
		INSERT INTO sage.agent_db_provision_attempts (
			deployment_id, kind, status, runner, command, exit_code,
			stdout, stderr, detail, finished_at
		)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9::jsonb, $10)
		RETURNING attempt_id, deployment_id, kind, status, runner, command,
			exit_code, stdout, stderr, detail, created_at, finished_at`,
		id,
		input.Kind,
		input.Status,
		input.Runner,
		jsonArray(input.Command),
		input.ExitCode,
		input.Stdout,
		input.Stderr,
		jsonBytes(input.Detail),
		finishedAt,
	), &attempt)
	return attempt, err
}

func scanProvisionAttempt(row scanner, attempt *ProvisionAttempt) error {
	return row.Scan(
		&attempt.AttemptID,
		&attempt.DeploymentID,
		&attempt.Kind,
		&attempt.Status,
		&attempt.Runner,
		&attempt.Command,
		&attempt.ExitCode,
		&attempt.Stdout,
		&attempt.Stderr,
		&attempt.Detail,
		&attempt.CreatedAt,
		&attempt.FinishedAt,
	)
}

func commandsFromPlan(plan map[string]any) ([]ProviderCommand, error) {
	raw, ok := plan["commands"].([]any)
	if !ok || len(raw) == 0 {
		return nil, ErrInvalid
	}
	commands := make([]ProviderCommand, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, ErrInvalid
		}
		args := stringsFromAny(m["args"])
		if len(args) == 0 {
			return nil, ErrInvalid
		}
		tool, _ := m["tool"].(string)
		if tool == "" {
			tool = args[0]
		}
		commands = append(commands, ProviderCommand{Tool: tool, Args: args})
	}
	return commands, nil
}

func commandArgs(commands []ProviderCommand) []string {
	if len(commands) == 0 {
		return nil
	}
	return commands[0].Args
}

func jsonArray(values []string) []byte {
	b, err := json.Marshal(values)
	if err != nil {
		return []byte(`[]`)
	}
	return b
}
