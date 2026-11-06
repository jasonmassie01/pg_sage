package briefing

import (
	"strings"
	"testing"
)

// Post-test audit (llm-08): code layouts an LLM writes that the first rules
// let through. Each must lose its invented statement.
func TestGroundCodeLayoutsCannotHideStatements(t *testing.T) {
	p := groundPayload(t, groundFindings)
	cases := map[string]string{
		"fence nested in a list item": "1. For finding #12:\n\n     ```sql\n" +
			"     VACUUM FULL orders;\n     ```\n",
		"tilde fence":         "~~~\nDROP TABLE x;\n~~~\n",
		"fence left open":     "```sql\nTRUNCATE orders;\n",
		"prefixed statement":  "```\nStep 1: DROP TABLE x;\n```\n",
		"untagged shell line": "```\npsql --command=\"VACUUM FULL orders\"\n```\n",
		"quoted shell line":   "```\npsql -c 'DROP TABLE x'\n```\n",
		"inline prefixed":     "Run `then: ALTER SYSTEM SET fsync = off` now.",
		"CRLF fence":          "```sql\r\nDROP TABLE x;\r\n```\r\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got, removed := groundOutput(text, p)
			if len(removed) != 1 {
				t.Fatalf("removed %q, want one statement:\n%s", removed, got)
			}
			for _, word := range []string{"VACUUM", "DROP", "TRUNCATE", "ALTER"} {
				if strings.Contains(got, word) {
					t.Fatalf("%s survives:\n%s", word, got)
				}
			}
			if !strings.Contains(got, "removed: not a pg_sage recommendation") {
				t.Fatalf("no removal marker:\n%s", got)
			}
		})
	}
}

// Code that changes nothing stays as written.
func TestGroundKeepsNonChangingCode(t *testing.T) {
	p := groundPayload(t, groundFindings)
	for _, text := range []string{
		"```\n| relation | dead_tup |\n| orders | 120000 |\n```\n",
		"Watch `pg_stat_user_tables.n_dead_tup` and `autovacuum_vacuum_scale_factor`.",
		"Index `public.update_log_idx` is unused.",
		"```sql\n-- online, see finding #12\n" + groundIndexSQL + ";\n```\n",
	} {
		if got, removed := groundOutput(text, p); got != text || len(removed) != 0 {
			t.Errorf("non-changing code rewritten (%q):\n%s", removed, got)
		}
	}
}

// Review (llm-08): the Problem names invented GUC changes. A setting written
// as a configuration line, a server option or a config-file fence is as much
// a change as ALTER SYSTEM, and pg_sage recommends none of them.
func TestGroundRemovesInventedSettings(t *testing.T) {
	p := groundPayload(t, groundFindings)
	cases := map[string]string{
		"conf line in a fence":  "```\nshared_buffers = 8GB\n```\n",
		"bare name in a fence":  "```\nfsync=off\n```\n",
		"inline assignment":     "Raise `work_mem = 256MB` for finding #12.",
		"server option":         "Restart with `postgres -c max_connections=500`.",
		"dotted extension name": "```\npg_stat_statements.max = 10000\n```\n",
		"ini fence":             "```ini\n[postgresql]\nwal_level: logical\n```\n",
		"yaml fence":            "```yaml\nparameters:\n  wal_level: logical\n```\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			got, removed := groundOutput(text, p)
			if len(removed) != 1 {
				t.Fatalf("removed %q, want one setting:\n%s", removed, got)
			}
			for _, word := range []string{"8GB", "fsync", "256MB", "500", "10000",
				"logical"} {
				if strings.Contains(got, word) {
					t.Fatalf("%s survives:\n%s", word, got)
				}
			}
			if !strings.Contains(got, "removed: not a pg_sage recommendation") {
				t.Fatalf("no removal marker:\n%s", got)
			}
		})
	}
}

// Comparisons and names are not assignments.
func TestGroundKeepsComparisonsAndSettingNames(t *testing.T) {
	p := groundPayload(t, groundFindings)
	for _, text := range []string{
		"Alert when `n_dead_tup >= 100000` or `ratio <= 0.9` or `a != b`.",
		"Check `x == y` and `key => value`.",
		"Raise `work_mem` and watch `pg_stat_statements.max`.",
		"```text\nwork_mem is 4MB; shared_buffers is 128MB\n```\n",
	} {
		if got, removed := groundOutput(text, p); got != text || len(removed) != 0 {
			t.Errorf("non-assignment code rewritten (%q):\n%s", removed, got)
		}
	}
}
