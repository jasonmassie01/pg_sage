package logwatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// lcMessagesQuery reads the server's message language. It is a read of
// pg_settings only.
const lcMessagesQuery = `SELECT setting FROM pg_settings WHERE name = 'lc_messages'`

// DetectLCMessages returns the server's lc_messages setting.
func DetectLCMessages(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var setting string
	if err := pool.QueryRow(ctx, lcMessagesQuery).Scan(&setting); err != nil {
		return "", fmt.Errorf("logwatch detect: read lc_messages: %w", err)
	}
	return setting, nil
}

// LCMessagesWarning returns an operator warning when lc_messages is not C
// or English, or "" when it is. SQLSTATE rules work in any language, but
// the text rules (crash, WAL gap, slot invalidation, wraparound, temp
// files, checkpoints) only match the English messages.
func LCMessagesWarning(setting string) string {
	if setting == "" {
		return "lc_messages is empty, so the server logs in its environment's " +
			"language; set lc_messages = 'C' (or an English locale) so log-based " +
			"RCA can match crash, WAL-gap, slot and wraparound messages"
	}
	if englishOrC(setting) {
		return ""
	}
	return fmt.Sprintf("lc_messages is %q: log-based RCA matches only C or English "+
		"messages, so crash, WAL-gap, slot and wraparound signals will be missed; "+
		"set lc_messages = 'C' (SQLSTATE-based signals still work)", setting)
}

// englishOrC accepts C, POSIX, C.<codeset>, en, en_<territory>[.codeset],
// en-<territory> and Windows "English_<territory>.<codepage>" names.
func englishOrC(setting string) bool {
	s := strings.ToLower(setting)
	switch {
	case s == "c", s == "posix", strings.HasPrefix(s, "c."):
		return true
	case s == "en", strings.HasPrefix(s, "en_"), strings.HasPrefix(s, "en-"),
		strings.HasPrefix(s, "en."):
		return true
	case s == "english", strings.HasPrefix(s, "english_"):
		return true
	}
	return false
}
