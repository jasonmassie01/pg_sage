package config

import "fmt"

// DefaultRetentionAuthAuditDays keeps the sign-in audit trail a year: long
// enough for a yearly access review, bounded on a busy SSO tenant.
const DefaultRetentionAuthAuditDays = 365

const maxRetentionAuthAuditDays = 3650

// validateAuthAuditDays refuses a window outside 0-3650 (0 keeps the trail).
func (r RetentionConfig) validateAuthAuditDays() error {
	if r.AuthAuditDays < 0 || r.AuthAuditDays > maxRetentionAuthAuditDays {
		return fmt.Errorf("retention.auth_audit_days must be 0-%d, got %d",
			maxRetentionAuthAuditDays, r.AuthAuditDays)
	}
	return nil
}
