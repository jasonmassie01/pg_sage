package cloudtel

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

// Backup records (drill-01b): manual snapshots, which outlive the
// instance, and the automated backups' restore window. One page of at
// most maxBackupRecords each (the API's minimum page size).

const maxBackupRecords = 20

// manualSnapshots returns the create times of the available manual
// snapshots of the instance, or of the cluster when cluster is set, and
// whether the page left more unlisted. A snapshot without a create time,
// or not yet available, is left out.
func (a *rdsAPI) manualSnapshots(ctx context.Context, instance,
	cluster string) ([]time.Time, bool, error) {
	if cluster != "" {
		out, err := a.client.DescribeDBClusterSnapshots(ctx,
			&rds.DescribeDBClusterSnapshotsInput{DBClusterIdentifier: aws.String(cluster),
				SnapshotType: aws.String("manual"), MaxRecords: aws.Int32(maxBackupRecords)})
		if err != nil {
			return nil, false, sdkError(ctx, "RDS DescribeDBClusterSnapshots", err)
		}
		var times []time.Time
		for _, s := range out.DBClusterSnapshots {
			times = appendAvailable(times, s.Status, s.SnapshotCreateTime)
		}
		return times, aws.ToString(out.Marker) != "", nil
	}
	out, err := a.client.DescribeDBSnapshots(ctx, &rds.DescribeDBSnapshotsInput{
		DBInstanceIdentifier: aws.String(instance), SnapshotType: aws.String("manual"),
		MaxRecords: aws.Int32(maxBackupRecords)})
	if err != nil {
		return nil, false, sdkError(ctx, "RDS DescribeDBSnapshots", err)
	}
	var times []time.Time
	for _, s := range out.DBSnapshots {
		times = appendAvailable(times, s.Status, s.SnapshotCreateTime)
	}
	return times, aws.ToString(out.Marker) != "", nil
}

func appendAvailable(times []time.Time, status *string, created *time.Time) []time.Time {
	if created == nil || !strings.EqualFold(aws.ToString(status), "available") {
		return times
	}
	return append(times, *created)
}

// restoreWindow is the instance's active automated backup's restore
// window (latest minus earliest restorable time); nil when there is no
// active backup with both ends.
func (a *rdsAPI) restoreWindow(ctx context.Context, resourceID string) (*time.Duration,
	error) {
	out, err := a.client.DescribeDBInstanceAutomatedBackups(ctx,
		&rds.DescribeDBInstanceAutomatedBackupsInput{DbiResourceId: aws.String(resourceID),
			MaxRecords: aws.Int32(maxBackupRecords)})
	if err != nil {
		return nil, sdkError(ctx, "RDS DescribeDBInstanceAutomatedBackups", err)
	}
	for _, b := range out.DBInstanceAutomatedBackups {
		if !strings.EqualFold(aws.ToString(b.Status), "active") {
			continue
		}
		if w := windowOf(b.RestoreWindow); w != nil {
			return w, nil
		}
	}
	return nil, nil
}

func windowOf(w *types.RestoreWindow) *time.Duration {
	if w == nil || w.EarliestTime == nil || w.LatestTime == nil ||
		w.LatestTime.Before(*w.EarliestTime) {
		return nil
	}
	d := w.LatestTime.Sub(*w.EarliestTime)
	return &d
}
