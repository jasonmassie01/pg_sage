package store

import (
	"errors"
	"fmt"
)

// ErrValidation marks an error caused by user-supplied input that
// failed field-level validation. Handlers should surface these as a
// 400 response with the message exposed — the caller needs it to fix
// the request. Wrap with fmt.Errorf("%w: ...", ErrValidation, ...)
// so callers can match via errors.Is.
var ErrValidation = errors.New("validation failed")

// ErrNotFound marks a valid request for a record that does not exist.
var ErrNotFound = errors.New("not found")

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
		return fmt.Errorf("%w: name is required", ErrValidation)
	}
	if len(input.Name) > 63 {
		return fmt.Errorf(
			"%w: name exceeds 63 characters", ErrValidation)
	}
	if input.Host == "" {
		return fmt.Errorf("%w: host is required", ErrValidation)
	}
	if input.Port < 1 || input.Port > 65535 {
		return fmt.Errorf("%w: port must be 1-65535", ErrValidation)
	}
	if input.DatabaseName == "" {
		return fmt.Errorf(
			"%w: database_name is required", ErrValidation)
	}
	if input.Username == "" {
		return fmt.Errorf(
			"%w: username is required", ErrValidation)
	}
	if requirePassword && input.Password == "" {
		return fmt.Errorf(
			"%w: password is required", ErrValidation)
	}
	if !validSSLModes[input.SSLMode] {
		return fmt.Errorf(
			"%w: sslmode must be one of "+
				"disable, allow, prefer, require, verify-ca, verify-full",
			ErrValidation,
		)
	}
	if !validTrustLevels[input.TrustLevel] {
		return fmt.Errorf(
			"%w: trust_level must be one of "+
				"observation, advisory, autonomous",
			ErrValidation,
		)
	}
	if !validExecutionModes[input.ExecutionMode] {
		return fmt.Errorf(
			"%w: execution_mode must be one of "+
				"auto, approval, manual",
			ErrValidation,
		)
	}
	return nil
}
