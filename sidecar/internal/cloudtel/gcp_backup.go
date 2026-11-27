package cloudtel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// Cloud SQL backup freshness (market-07): the instance description has
// the backup configuration only, so the results come from one bounded
// backupRuns.list (the 5 latest runs), read at most every 15 minutes.
// A failed read keeps the configuration and marks freshness unavailable.

const (
	backupRunsMax     = 5
	backupRunsRefresh = 15 * time.Minute
)

// backupRunsResult is one read of an instance's backup runs.
type backupRunsResult struct {
	name     string
	at       time.Time
	last     *time.Time
	noRecent bool
	reason   string
}

// backupFreshness returns the instance's backup runs as last read, reading
// them again when the cached read is older than backupRunsRefresh. A read
// cut short by the caller's context is returned but not cached.
func (s *GCPSource) backupFreshness(ctx context.Context, name string,
	now time.Time) backupRunsResult {
	s.mu.Lock()
	cached := s.backupRuns
	s.mu.Unlock()
	if cached != nil && cached.name == name && !now.Before(cached.at) &&
		now.Sub(cached.at) < backupRunsRefresh {
		return *cached
	}
	res := s.readBackupRuns(ctx, name)
	if ctx.Err() != nil {
		return backupRunsResult{reason: "backup runs read cancelled"}
	}
	res.name, res.at = name, now
	s.mu.Lock()
	s.backupRuns = &res
	s.mu.Unlock()
	return res
}

func (s *GCPSource) readBackupRuns(ctx context.Context, name string) backupRunsResult {
	endpoint := s.opts.SQLAdminEndpoint + "/v1/projects/" + url.PathEscape(s.opts.Project) +
		"/instances/" + url.PathEscape(name) + "/backupRuns?maxResults=" +
		strconv.Itoa(backupRunsMax)
	raw, err := s.get(ctx, endpoint, "Cloud SQL Admin backupRuns.list")
	if err != nil {
		return backupRunsResult{reason: sanitize(err.Error())}
	}
	var list struct {
		Items []struct {
			Status  string `json:"status"`
			EndTime string `json:"endTime"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return backupRunsResult{reason: fmt.Sprintf("%v: Cloud SQL backup runs JSON",
			ErrMalformed)}
	}
	var res backupRunsResult
	failed := false
	for i, run := range list.Items {
		if i >= backupRunsMax {
			break
		}
		switch run.Status {
		case "SUCCESSFUL":
			end, err := time.Parse(time.RFC3339Nano, run.EndTime)
			if err == nil && (res.last == nil || end.After(*res.last)) {
				res.last = &end
			}
		case "FAILED":
			failed = true
		}
	}
	res.noRecent = failed && res.last == nil
	return res
}

// applyBackupFreshness records the backup runs on the sample's posture.
func (s *GCPSource) applyBackupFreshness(ctx context.Context, sample *Sample, name string,
	now time.Time) {
	if sample.Backup == nil {
		sample.Backup = &BackupPosture{}
	}
	fresh := s.backupFreshness(ctx, name, now)
	sample.Backup.LastSuccessfulBackup = clonePtr(fresh.last)
	sample.Backup.NoRecentSuccess = fresh.noRecent
	sample.Backup.FreshnessUnavailable = fresh.reason
}
