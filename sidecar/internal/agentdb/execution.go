package agentdb

import (
	"context"
	"encoding/json"
	"errors"
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

func (s *Store) ExecuteProvisionLive(
	ctx context.Context,
	id string,
	runner ProviderRunner,
	req LiveExecutionRequest,
) (ProvisionAttempt, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if runner == nil || runner.Name() == "dry_run" {
		return ProvisionAttempt{}, ErrInvalid
	}
	if req.CostEstimateID == "" {
		return ProvisionAttempt{}, ErrInvalid
	}
	if dep.ProvisioningStatus == "available" {
		return ProvisionAttempt{}, ErrConflict
	}
	if dep.ProvisioningStatus != "preflight_passed" &&
		dep.ProvisioningStatus != "failed" &&
		dep.ProvisioningStatus != "dry_run_ready" &&
		dep.ProvisioningStatus != "status_checked" {
		return ProvisionAttempt{}, ErrInvalid
	}
	commands, err := commandsFromPlan(dep.ProvisioningPlan)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.updateProvisioningStatus(ctx, id, "provisioning", nil); err != nil {
		return ProvisionAttempt{}, err
	}
	provReq := ProvisionRequest{
		Operation:     ProvisionOpCreate,
		Deployment:    dep,
		Plan:          dep.ProvisioningPlan,
		Policy:        req.Policy,
		RequestedAt:   time.Now().UTC(),
		DryRunCommand: commands[0],
	}
	result := runner.Create(ctx, provReq)
	status := "succeeded"
	nextStatus := firstNonEmpty(result.Status, "available")
	if result.Error != nil {
		status = "failed"
		nextStatus = "failed"
	}
	detail := RedactProviderDetail(result.Detail)
	detail["mode"] = "live"
	detail["cost_estimate_id"] = req.CostEstimateID
	attempt, err := s.recordProvisionAttempt(ctx, id, provisionAttemptInput{
		Kind:       "execute_live",
		Status:     status,
		Runner:     runner.Name(),
		Command:    commands[0].Args,
		Detail:     detail,
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if result.ProviderResourceID != "" {
		if err := s.RecordCreationReceipt(ctx, CreationReceipt{
			DeploymentID:       id,
			Provider:           dep.Provider,
			ProviderResourceID: result.ProviderResourceID,
			OperationMode:      "live",
			RequestHash:        req.CostEstimateID,
			Detail:             detail,
		}); err != nil {
			return ProvisionAttempt{}, err
		}
	}
	if err := s.applyProvisionResult(ctx, id, nextStatus, result); err != nil {
		return ProvisionAttempt{}, err
	}
	_ = s.audit(ctx, id, "provision_execute_live_"+status, detail)
	if result.Error != nil {
		return attempt, publicProviderError(result.Error)
	}
	return attempt, nil
}

func (s *Store) DestroyProvisionLive(
	ctx context.Context,
	id string,
	runner ProviderRunner,
) (ProvisionAttempt, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if runner == nil || runner.Name() == "dry_run" {
		return ProvisionAttempt{}, ErrInvalid
	}
	if err := s.requireRestoreVerifiedBackup(ctx, dep); err != nil {
		return ProvisionAttempt{}, err
	}
	if dep.ProvisioningStatus != "available" &&
		dep.ProvisioningStatus != "status_checked" &&
		dep.ProvisioningStatus != "dry_run_ready" {
		return ProvisionAttempt{}, ErrInvalid
	}
	if err := s.updateProvisioningStatus(ctx, id, "destroy_pending", nil); err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.updateProvisioningStatus(ctx, id, "destroying", nil); err != nil {
		return ProvisionAttempt{}, err
	}
	result := runner.Destroy(ctx, ProvisionRequest{
		Operation:   ProvisionOpDestroy,
		Deployment:  dep,
		Plan:        dep.ProvisioningPlan,
		RequestedAt: time.Now().UTC(),
	})
	status := "succeeded"
	nextStatus := firstNonEmpty(result.Status, "destroying")
	if result.Error != nil {
		status = "failed"
		nextStatus = "failed"
	}
	attempt, err := s.recordProvisionAttempt(ctx, id, provisionAttemptInput{
		Kind:       "destroy_live",
		Status:     status,
		Runner:     runner.Name(),
		Detail:     result.Detail,
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.applyProvisionResult(ctx, id, nextStatus, result); err != nil {
		return ProvisionAttempt{}, err
	}
	_ = s.audit(ctx, id, "provision_destroy_live_"+status, attempt.Detail)
	if result.Error != nil {
		return attempt, publicProviderError(result.Error)
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

func (s *Store) CheckProvisionStatusLive(
	ctx context.Context,
	id string,
	runner ProviderRunner,
) (ProvisionAttempt, error) {
	dep, err := s.cloudDeploymentForExecution(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if runner == nil || runner.Name() == "dry_run" {
		return ProvisionAttempt{}, ErrInvalid
	}
	result := runner.Status(ctx, ProvisionRequest{
		Operation:   ProvisionOpStatus,
		Deployment:  dep,
		Plan:        dep.ProvisioningPlan,
		RequestedAt: time.Now().UTC(),
	})
	status := "succeeded"
	nextStatus := firstNonEmpty(result.Status, "status_checked")
	providerErr := publicProviderError(result.Error)
	if result.Error != nil {
		if errors.Is(providerErr, ErrNotFound) &&
			dep.ProvisioningStatus == "destroying" {
			nextStatus = "destroyed"
		} else {
			status = "failed"
			nextStatus = "status_unknown"
		}
	}
	detail := RedactProviderDetail(result.Detail)
	attempt, err := s.recordProvisionAttempt(ctx, id, provisionAttemptInput{
		Kind:       "status_check_live",
		Status:     status,
		Runner:     runner.Name(),
		Detail:     detail,
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return ProvisionAttempt{}, err
	}
	if err := s.applyProvisionResult(ctx, id, nextStatus, result); err != nil {
		return ProvisionAttempt{}, err
	}
	_ = s.audit(ctx, id, "provision_status_live_"+status, detail)
	if result.Error != nil {
		if errors.Is(providerErr, ErrNotFound) &&
			dep.ProvisioningStatus == "destroying" {
			return attempt, nil
		}
		return attempt, providerErr
	}
	return attempt, nil
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
	var current string
	if err := s.pool.QueryRow(ctx, `
		SELECT provisioning_status
		FROM sage.agent_db_deployments
		WHERE deployment_id=$1`, id).Scan(&current); err != nil {
		return err
	}
	if err := requireProvisionTransition(current, status); err != nil {
		return err
	}
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

func (s *Store) applyProvisionResult(
	ctx context.Context,
	id string,
	status string,
	result ProvisionResult,
) error {
	if result.ConnectionInfo == nil {
		result.ConnectionInfo = map[string]any{}
	}
	var current string
	if err := s.pool.QueryRow(ctx, `
		SELECT provisioning_status
		FROM sage.agent_db_deployments
		WHERE deployment_id=$1`, id).Scan(&current); err != nil {
		return err
	}
	if err := requireProvisionTransition(current, status); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE sage.agent_db_deployments
		SET provisioning_status=$2,
			provider_resource_id=COALESCE(NULLIF($3, ''), provider_resource_id),
			secret_ref=COALESCE(NULLIF($4, ''), secret_ref),
			secret_ref_provider=COALESCE(NULLIF($5, ''), secret_ref_provider),
			live_mode=true,
			connection_info=connection_info || $6::jsonb,
			updated_at=now()
		WHERE deployment_id=$1`,
		id,
		status,
		result.ProviderResourceID,
		result.SecretRef,
		result.SecretRefProvider,
		jsonBytes(RedactProviderDetail(result.ConnectionInfo)),
	)
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
	input.Detail = RedactProviderDetail(input.Detail)
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

func (s *Store) RecordCreationReceipt(
	ctx context.Context,
	receipt CreationReceipt,
) error {
	if err := s.Ensure(ctx); err != nil {
		return err
	}
	receipt.Detail = RedactProviderDetail(receipt.Detail)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sage.agent_db_creation_receipts (
			deployment_id, provider, provider_resource_id, region, account_ref,
			request_hash, operation_mode, detail
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
		ON CONFLICT (deployment_id) DO UPDATE
		SET provider=EXCLUDED.provider,
			provider_resource_id=EXCLUDED.provider_resource_id,
			region=EXCLUDED.region,
			account_ref=EXCLUDED.account_ref,
			request_hash=EXCLUDED.request_hash,
			operation_mode=EXCLUDED.operation_mode,
			detail=EXCLUDED.detail,
			updated_at=now()`,
		receipt.DeploymentID,
		receipt.Provider,
		receipt.ProviderResourceID,
		receipt.Region,
		receipt.AccountRef,
		receipt.RequestHash,
		receipt.OperationMode,
		jsonBytes(receipt.Detail),
	)
	return err
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
