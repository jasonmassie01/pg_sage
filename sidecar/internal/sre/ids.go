// Package sre is the Sage SRE coordination foundation (AI-SRE-SPEC §5,
// §10, §11; Codex contracts §1-§6): durable investigations with leases
// and fencing, idempotent steps, immutable probe evidence, durable model
// budget reservations, the metadata-durability guard and the claim
// validator for evidence-bound narration.
package sre

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// UUID is a canonical lower-case UUID string, generated in Go.
type UUID string

var uuidPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewUUID returns a random (version 4) UUID.
func NewUUID() UUID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("sre: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return UUID(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:])
}

// ParseUUID validates s and returns it in canonical lower case.
func ParseUUID(s string) (UUID, error) {
	low := strings.ToLower(s)
	if !uuidPattern.MatchString(low) {
		return "", fmt.Errorf("%w: invalid uuid %q", ErrInvalidRequest, s)
	}
	return UUID(low), nil
}

// Scope keys every SRE row: the deployment and the stable database
// identity. Names, aliases and query ids never scope data.
type Scope struct {
	DeploymentID UUID
	DatabaseID   UUID
}

// Validate requires both ids to be canonical UUIDs.
func (s Scope) Validate() error {
	if _, err := ParseUUID(string(s.DeploymentID)); err != nil {
		return fmt.Errorf("deployment id: %w", err)
	}
	if _, err := ParseUUID(string(s.DatabaseID)); err != nil {
		return fmt.Errorf("database id: %w", err)
	}
	return nil
}

// Store errors. Callers distinguish them with errors.Is.
var (
	ErrInvalidRequest      = errors.New("invalid request")
	ErrNotFound            = errors.New("investigation not found")
	ErrLeaseUnavailable    = errors.New("investigation is leased by another worker")
	ErrLeaseLost           = errors.New("lease lost")
	ErrTerminal            = errors.New("investigation is terminal")
	ErrInvalidTransition   = errors.New("invalid state transition")
	ErrVersionConflict     = errors.New("version conflict")
	ErrBudgetExhausted     = errors.New("investigation budget exhausted")
	ErrUsageExceeded       = errors.New("provider usage exceeded the reservation")
	ErrMetadataUnavailable = errors.New("metadata store unavailable")
)
