package clone

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Upgrade (drill-02b) runs upgrade_argv for the rehearsal clone cl with
// SAGE_CLONE_ID and SAGE_TARGET_MAJOR, bounded by the max duration. A
// program that prints nothing upgraded the clone in place; one that
// prints {"dsn"} on one line moved it there (never the source). The
// rehearsal destroys the clone afterwards under the same id.
func (p *CommandProvider) Upgrade(ctx context.Context, cl Clone, targetMajor int) (Clone,
	error) {
	if len(p.cfg.UpgradeArgv) == 0 {
		return Clone{}, ErrCommandNoUpgrade
	}
	if err := validCommandCloneID(cl.ID); err != nil {
		return Clone{}, err
	}
	if targetMajor < 1 || targetMajor > 1000 {
		return Clone{}, fmt.Errorf("%w: target major %d is not a PostgreSQL major",
			ErrCommandInvalidConfig, targetMajor)
	}
	out, err := p.run(ctx, p.cfg.UpgradeArgv, p.cfg.MaxDuration, "max_duration_minutes",
		"SAGE_CLONE_ID", cl.ID, "SAGE_TARGET_MAJOR="+strconv.Itoa(targetMajor))
	if err != nil {
		return Clone{}, err
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return cl, nil
	}
	dsn, err := parseUpgradeLine(text, p.cfg.SourceDSN)
	if err != nil {
		return Clone{}, err
	}
	return Clone{DSN: dsn, ID: cl.ID, CreatedFrom: cl.CreatedFrom}, nil
}

// parseUpgradeLine reads the upgrade program's one JSON line {"dsn"}.
func parseUpgradeLine(text, source string) (string, error) {
	var line struct {
		DSN string `json:"dsn"`
	}
	dec := json.NewDecoder(strings.NewReader(text))
	if strings.ContainsAny(text, "\r\n") || dec.Decode(&line) != nil || dec.More() {
		return "", fmt.Errorf("%w: the upgrade program printed %q, want nothing or one "+
			"line {\"dsn\": ...}", ErrCommandBadOutput, snippet(text))
	}
	if strings.TrimSpace(line.DSN) == "" {
		return "", fmt.Errorf("%w: dsn", ErrCommandMissingField)
	}
	if err := refuseSourceDSN(line.DSN, source); err != nil {
		return "", err
	}
	return line.DSN, nil
}
