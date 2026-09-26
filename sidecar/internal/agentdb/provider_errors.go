package agentdb

import (
	"errors"
	"fmt"
	"strings"
)

type ProviderErrorKind string

const (
	ProviderErrInvalid      ProviderErrorKind = "invalid"
	ProviderErrConflict     ProviderErrorKind = "conflict"
	ProviderErrQuota        ProviderErrorKind = "quota"
	ProviderErrPermission   ProviderErrorKind = "permission"
	ProviderErrThrottle     ProviderErrorKind = "throttle"
	ProviderErrUnavailable  ProviderErrorKind = "unavailable"
	ProviderErrNotFound     ProviderErrorKind = "not_found"
	ProviderErrNeedsSecrets ProviderErrorKind = "needs_secrets"
)

type ProviderError struct {
	Provider string            `json:"provider"`
	Kind     ProviderErrorKind `json:"kind"`
	Message  string            `json:"message"`
	Hint     string            `json:"hint"`
}

func (e ProviderError) Error() string {
	if e.Hint == "" {
		return fmt.Sprintf("%s provider %s: %s", e.Provider, e.Kind, e.Message)
	}
	return fmt.Sprintf("%s provider %s: %s (%s)", e.Provider, e.Kind, e.Message, e.Hint)
}

func providerError(provider string, kind ProviderErrorKind, msg string, hint string) error {
	return ProviderError{Provider: provider, Kind: kind, Message: msg, Hint: hint}
}

func publicProviderError(err error) error {
	if err == nil {
		return nil
	}
	if pe, ok := err.(ProviderError); ok {
		switch pe.Kind {
		case ProviderErrConflict:
			return ErrConflict
		case ProviderErrNotFound:
			return ErrNotFound
		case ProviderErrThrottle:
			return ErrRateLimited
		default:
			return ErrInvalid
		}
	}
	return err
}

// httpStatusKind classifies provider HTTP failures by status code rather
// than by substrings of the message (G8-B22).
func httpStatusKind(status int) ProviderErrorKind {
	switch {
	case status == 401 || status == 403:
		return ProviderErrPermission
	case status == 404:
		return ProviderErrNotFound
	case status == 409:
		return ProviderErrConflict
	case status == 429:
		return ProviderErrThrottle
	case status == 400 || status == 422:
		return ProviderErrInvalid
	default:
		return ProviderErrUnavailable
	}
}

func mapProviderError(provider string, err error) error {
	if err == nil {
		return nil
	}
	var typed ProviderError
	if errors.As(err, &typed) {
		return typed
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "already") || strings.Contains(msg, "exists"):
		return providerError(provider, ProviderErrConflict, err.Error(), "")
	case strings.Contains(msg, "throttl") || strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "rate exceeded") || strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "status 429"):
		return providerError(provider, ProviderErrThrottle, err.Error(), "retry later")
	case strings.Contains(msg, "quota") || strings.Contains(msg, "limit"):
		return providerError(provider, ProviderErrQuota, err.Error(), "request quota")
	case strings.Contains(msg, "permission") || strings.Contains(msg, "denied") ||
		strings.Contains(msg, "forbidden") || strings.Contains(msg, "unauthorized"):
		return providerError(provider, ProviderErrPermission, err.Error(), "check IAM")
	case strings.Contains(msg, "notfound") || strings.Contains(msg, "not found") ||
		strings.Contains(msg, "404"):
		return providerError(provider, ProviderErrNotFound, err.Error(), "")
	case strings.Contains(msg, "invalid"):
		return providerError(provider, ProviderErrInvalid, err.Error(), "")
	default:
		return providerError(provider, ProviderErrUnavailable, err.Error(), "")
	}
}
