package agentposture

// Platform is where the database runs, for the detectors whose evidence
// lives outside PostgreSQL (AP-11): the detected cloud environment and,
// when cloud telemetry reads it, the provider's backup posture.
type Platform struct {
	// Provider is the detected environment: "self-managed", "rds",
	// "aurora", "cloud-sql", "alloydb", "azure", "neon", "supabase", ...;
	// "" or "unknown" when it was not detected.
	Provider string
	// Backup is the provider's backup posture from cloud telemetry; nil
	// when telemetry is off, failing or not available for the provider.
	Backup *BackupPosture
}

// BackupPosture is what the provider's instance description says about
// recovery. A nil field is unknown, never "off".
type BackupPosture struct {
	BackupsEnabled     *bool
	PITREnabled        *bool
	RetentionDays      *int
	DeletionProtection *bool
}

// selfManaged reports whether PostgreSQL's own settings are the recovery
// evidence: a self-managed server, or one whose environment is unknown.
func (p Platform) selfManaged() bool {
	switch p.Provider {
	case "", "unknown", "self-managed":
		return true
	}
	return false
}
