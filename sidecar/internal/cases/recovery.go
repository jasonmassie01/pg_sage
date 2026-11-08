package cases

import "strings"

// An action family may be reversible in principle without an attached recovery
// procedure for this proposal. Do not turn that potential into a UI guarantee.
func normalizeRecoveryClaims(candidates []ActionCandidate) {
	for i := range candidates {
		c := &candidates[i]
		if c.RollbackClass != "reversible" && c.RollbackClass != "unknown" {
			continue
		}
		if c.RollbackClass == "reversible" && c.ScriptOutput != nil && recoverySQLPresent(c.ScriptOutput.RollbackSQL) {
			continue
		}
		c.RollbackClass = "unknown"
		if c.RecoveryDetail == "" {
			c.RecoveryDetail = "No validated recovery procedure is attached to this proposal. Capture the previous state and review recovery before applying a change."
		}
		if c.ScriptOutput != nil {
			c.ScriptOutput.RollbackSQL = ""
			for j, label := range c.ScriptOutput.RiskLabels {
				if label == "reversible" {
					c.ScriptOutput.RiskLabels[j] = "unknown"
				}
			}
			c.ScriptOutput.PRBody = strings.ReplaceAll(c.ScriptOutput.PRBody, "Rollback class: reversible", "Rollback class: unknown")
		}
	}
}

func recoverySQLPresent(sql string) bool {
	// Skip both PostgreSQL comment forms, including nested block comments.
	// This tests presence only; each action family validates its own inverse.
	for sql = strings.TrimSpace(sql); sql != ""; sql = strings.TrimSpace(sql) {
		switch {
		case strings.HasPrefix(sql, "--"):
			end := strings.IndexByte(sql, '\n')
			if end < 0 {
				return false
			}
			sql = sql[end+1:]
		case strings.HasPrefix(sql, "/*"):
			depth, i := 1, 2
			for i < len(sql) && depth > 0 {
				switch {
				case strings.HasPrefix(sql[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(sql[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth != 0 {
				return false
			}
			sql = sql[i:]
		case sql[0] == ';':
			sql = sql[1:]
		default:
			return true
		}
	}
	return false
}
