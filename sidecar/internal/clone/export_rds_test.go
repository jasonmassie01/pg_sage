package clone

import "time"

// SetRDSRestoreDeadlineForTest shortens a's restore deadline below the
// configurable minimum, so a test can wait it out.
func SetRDSRestoreDeadlineForTest(a *RDSRestoreAPI, d time.Duration) { a.cfg.Deadline = d }

// SetCloudSQLDeadlineForTest shortens a's restore deadline below the
// configurable minimum, so a test can wait it out.
func SetCloudSQLDeadlineForTest(a *CloudSQLCloneAPI, d time.Duration) { a.cfg.Deadline = d }

// SetAlloyDBDeadlineForTest shortens a's restore deadline below the
// configurable minimum, so a test can wait it out.
func SetAlloyDBDeadlineForTest(a *AlloyDBRestoreAPI, d time.Duration) { a.cfg.Deadline = d }

// SetAzureRestoreDeadlineForTest shortens a's restore deadline below the
// configurable minimum, so a test can wait it out.
func SetAzureRestoreDeadlineForTest(a *AzureRestoreAPI, d time.Duration) { a.cfg.Deadline = d }
