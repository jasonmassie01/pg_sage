package clone

import "time"

// SetRDSRestoreDeadlineForTest shortens a's restore deadline below the
// configurable minimum, so a test can wait it out.
func SetRDSRestoreDeadlineForTest(a *RDSRestoreAPI, d time.Duration) { a.cfg.Deadline = d }

// SetCloudSQLDeadlineForTest shortens a's restore deadline below the
// configurable minimum, so a test can wait it out.
func SetCloudSQLDeadlineForTest(a *CloudSQLCloneAPI, d time.Duration) { a.cfg.Deadline = d }
