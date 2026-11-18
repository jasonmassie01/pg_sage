package changeplan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// MaxRawOutputBytes bounds one model pass's raw output (1 MiB), the
// sage.plan_model_record check.
const MaxRawOutputBytes = 1 << 20

// maxPromptVersionRunes bounds a prompt version, as sage.plan does.
const maxPromptVersionRunes = 100

// Errors RecordModelPass returns before the insert.
var (
	ErrRawOutputTooLarge = errors.New("changeplan: the raw model output is larger than 1 MiB")
	ErrInvalidModelPass  = errors.New("changeplan: invalid model pass")
)

// ModelPass is a sage.plan_model_record row: one model pass that
// contributed to a plan, the input a later model is replayed on. RawOutput
// is stored byte for byte (markdown fences included); RawOutputSHA256 is
// its SHA-256, computed by the store.
type ModelPass struct {
	ID              int64     `json:"id,omitempty"`
	PlanID          int64     `json:"plan_id"`
	PackHash        string    `json:"pack_hash,omitempty"`
	Model           string    `json:"model"`
	PromptVersion   string    `json:"prompt_version"`
	RawOutput       string    `json:"raw_output"`
	RawOutputSHA256 string    `json:"raw_output_sha256"`
	CreatedAt       time.Time `json:"created_at"`
}

func rawOutputSHA256(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// checkModelPass refuses what the table would refuse, with the reason, and
// what PostgreSQL text cannot hold byte for byte (invalid UTF-8, NUL).
func checkModelPass(m ModelPass) error {
	switch n := utf8.RuneCountInString(m.Model); {
	case len(m.RawOutput) > MaxRawOutputBytes:
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrRawOutputTooLarge,
			len(m.RawOutput), MaxRawOutputBytes)
	case n < 1 || n > maxRefRunes:
		return fmt.Errorf("%w: the model is 1 to %d characters", ErrInvalidModelPass,
			maxRefRunes)
	case utf8.RuneCountInString(m.PromptVersion) > maxPromptVersionRunes:
		return fmt.Errorf("%w: the prompt version is at most %d characters",
			ErrInvalidModelPass, maxPromptVersionRunes)
	case m.PackHash != "" && !packHashPattern.MatchString(m.PackHash):
		return fmt.Errorf("%w: pack hash %q is not 64 lower-case hex", ErrInvalidModelPass,
			m.PackHash)
	case !utf8.ValidString(m.RawOutput):
		return fmt.Errorf("%w: the raw output is not valid UTF-8", ErrInvalidModelPass)
	case strings.ContainsRune(m.RawOutput, 0):
		return fmt.Errorf("%w: the raw output holds a NUL byte", ErrInvalidModelPass)
	}
	return nil
}

// RecordModelPass stores one model pass of plan planID, hashing its raw
// output, and returns the stored row. The record is append-only and
// chained at commit (auditchain.PlanModelRecords).
func (s *Store) RecordModelPass(ctx context.Context, planID int64,
	m ModelPass) (ModelPass, error) {
	if planID <= 0 {
		return ModelPass{}, fmt.Errorf("%w: no plan", ErrInvalidModelPass)
	}
	if err := checkModelPass(m); err != nil {
		return ModelPass{}, fmt.Errorf("changeplan: record a model pass of plan %d: %w",
			planID, err)
	}
	m.PlanID, m.RawOutputSHA256 = planID, rawOutputSHA256(m.RawOutput)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.plan_model_record (plan_id,
			pack_hash, model, prompt_version, raw_output, raw_output_sha256)
			SELECT $1, NULLIF($2, ''), $3, $4, $5, $6
			 WHERE EXISTS (SELECT 1 FROM sage.plan WHERE id = $1)
			RETURNING id, created_at`, planID, m.PackHash, m.Model, m.PromptVersion,
			m.RawOutput, m.RawOutputSHA256).Scan(&m.ID, &m.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: plan %d", ErrNotFound, planID)
		}
		return err
	})
	if err != nil {
		return ModelPass{}, fmt.Errorf("changeplan: record a model pass of plan %d: %w",
			planID, err)
	}
	return m, nil
}
