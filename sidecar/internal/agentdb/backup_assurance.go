package agentdb

import (
	"context"
	"time"
)

func (s *Store) CheckBackupAssurance(
	ctx context.Context,
	id string,
	runner ProvisionRunner,
) (BackupAssurance, error) {
	dep, err := s.Get(ctx, id)
	if err != nil {
		return BackupAssurance{}, err
	}
	command, mode, err := providerBackupCheckCommand(dep)
	if err != nil {
		return BackupAssurance{}, err
	}
	attempt, err := s.runBackupCommand(ctx, dep, backupRunInput{
		Kind:    "backup_check",
		Command: command,
		Runner:  runner,
		Detail:  map[string]any{"mode": mode},
	})
	if err != nil {
		return BackupAssurance{}, err
	}
	backup, err := s.RecordBackup(ctx, id, BackupRequest{
		BackupID: "backup_check_" + idFrom(id, attempt.CreatedAt.String()),
		Provider: dep.Provider,
		Status:   "verified",
		Detail: map[string]any{
			"mode":       mode,
			"attempt_id": attempt.AttemptID,
		},
	})
	if err != nil {
		return BackupAssurance{}, err
	}
	backups, err := s.Backups(ctx, id)
	if err != nil {
		return BackupAssurance{}, err
	}
	return BackupAssurance{
		DeploymentID:   id,
		Mode:           mode,
		BackupStatus:   backup.Status,
		SafeForDestroy: hasRestoreVerifiedBackup(backups),
		Attempt:        attempt,
		Backup:         backup,
	}, nil
}

func (s *Store) CheckBackupAssuranceLive(
	ctx context.Context,
	id string,
	runner ProviderRunner,
) (BackupAssurance, error) {
	dep, err := s.Get(ctx, id)
	if err != nil {
		return BackupAssurance{}, err
	}
	if runner == nil || runner.Name() == "dry_run" {
		return BackupAssurance{}, ErrInvalid
	}
	result := runner.BackupCheck(ctx, ProvisionRequest{
		Operation:   ProvisionOpBackup,
		Deployment:  dep,
		Plan:        dep.ProvisioningPlan,
		RequestedAt: time.Now().UTC(),
	})
	status := "succeeded"
	if result.Error != nil {
		status = "failed"
	}
	detail := RedactProviderDetail(result.Detail)
	detail["mode"] = "live"
	attempt, err := s.recordProvisionAttempt(ctx, id, provisionAttemptInput{
		Kind:       "backup_check_live",
		Status:     status,
		Runner:     runner.Name(),
		Detail:     detail,
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return BackupAssurance{}, err
	}
	if result.Error != nil {
		return BackupAssurance{}, publicProviderError(result.Error)
	}
	backup, err := s.RecordBackup(ctx, id, BackupRequest{
		BackupID: "backup_check_live_" + idFrom(id, attempt.CreatedAt.String()),
		Provider: dep.Provider,
		Status:   "restore_verified",
		Detail: map[string]any{
			"mode":       "live",
			"attempt_id": attempt.AttemptID,
		},
	})
	if err != nil {
		return BackupAssurance{}, err
	}
	return BackupAssurance{
		DeploymentID:   id,
		Mode:           "live",
		BackupStatus:   backup.Status,
		SafeForDestroy: true,
		Attempt:        attempt,
		Backup:         backup,
	}, nil
}

func (s *Store) PlanRestoreDrillDryRun(
	ctx context.Context,
	id string,
	runner ProvisionRunner,
) (ProvisionAttempt, error) {
	dep, err := s.Get(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	backups, err := s.Backups(ctx, id)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	command, err := providerRestoreDrillCommand(dep, backups)
	if err != nil {
		return ProvisionAttempt{}, err
	}
	return s.runBackupCommand(ctx, dep, backupRunInput{
		Kind:    "restore_drill_dry_run",
		Command: command,
		Runner:  runner,
		Detail: map[string]any{
			"restore_verification": "not_granted_by_dry_run",
		},
	})
}

type backupRunInput struct {
	Kind    string
	Command ProviderCommand
	Runner  ProvisionRunner
	Detail  map[string]any
}

func (s *Store) runBackupCommand(
	ctx context.Context,
	dep Deployment,
	input backupRunInput,
) (ProvisionAttempt, error) {
	runner := input.Runner
	if runner == nil {
		runner = DryRunProvisionRunner{}
	}
	result := runner.Run(ctx, input.Command)
	detail := mergeDetail(input.Detail, result.Detail)
	status := "succeeded"
	if result.ExitCode != 0 {
		status = "failed"
	}
	attempt, err := s.recordProvisionAttempt(ctx, dep.DeploymentID, provisionAttemptInput{
		Kind:       input.Kind,
		Status:     status,
		Runner:     "dry_run",
		Command:    input.Command.Args,
		ExitCode:   result.ExitCode,
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		Detail:     detail,
		FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return ProvisionAttempt{}, err
	}
	_ = s.audit(ctx, dep.DeploymentID, input.Kind+"_"+status, detail)
	if result.ExitCode != 0 {
		return attempt, ErrInvalid
	}
	return attempt, nil
}

func mergeDetail(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}
