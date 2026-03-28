package store

import "fmt"

var validSSLModes = map[string]bool{
	"disable":     true,
	"allow":       true,
	"prefer":      true,
	"require":     true,
	"verify-ca":   true,
	"verify-full": true,
}

var validTrustLevels = map[string]bool{
	"observation": true,
	"advisory":    true,
	"autonomous":  true,
}

var validExecutionModes = map[string]bool{
	"auto":     true,
	"approval": true,
	"manual":   true,
}

// validateInput checks all fields of a DatabaseInput.
// requirePassword is true for create, false for update.
func validateInput(input DatabaseInput, requirePassword bool) error {
	if input.Name == "" {
		return fmt.Errorf("validate: name is required")
	}
	if len(input.Name) > 63 {
		return fmt.Errorf("validate: name exceeds 63 characters")
	}
	if input.Host == "" {
		return fmt.Errorf("validate: host is required")
	}
	if input.Port < 1 || input.Port > 65535 {
		return fmt.Errorf("validate: port must be 1-65535")
	}
	if input.DatabaseName == "" {
		return fmt.Errorf("validate: database_name is required")
	}
	if input.Username == "" {
		return fmt.Errorf("validate: username is required")
	}
	if requirePassword && input.Password == "" {
		return fmt.Errorf("validate: password is required")
	}
	if !validSSLModes[input.SSLMode] {
		return fmt.Errorf(
			"validate: sslmode must be one of "+
				"disable, allow, prefer, require, verify-ca, verify-full",
		)
	}
	if !validTrustLevels[input.TrustLevel] {
		return fmt.Errorf(
			"validate: trust_level must be one of "+
				"observation, advisory, autonomous",
		)
	}
	if !validExecutionModes[input.ExecutionMode] {
		return fmt.Errorf(
			"validate: execution_mode must be one of "+
				"auto, approval, manual",
		)
	}
	return nil
}
