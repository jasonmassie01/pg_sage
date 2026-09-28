package plan

import (
	"fmt"
	"regexp"
	"strings"
)

const planIdent = `(?:"(?:[^"]|"")+"|[\w$]+)`

var alterTargetPattern = regexp.MustCompile(
	`(?i)^\s*ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?` +
		`(?:(` + planIdent + `)\.)?(` + planIdent + `)`)

// validateStatement refuses anything the planner would only partially
// plan (G7-B24): multiple statements, multiple ALTER TABLE clauses, or
// SQL whose target table differs from the table the caller described.
func validateStatement(request Request) error {
	sql := strings.TrimRight(strings.TrimSpace(request.SQL), "; \t\r\n")
	if topLevelCount(sql, ';') > 0 || topLevelCount(sql, ',') > 0 {
		return fmt.Errorf("unsupported migration statement: the planner " +
			"handles a single ALTER TABLE clause; split the migration")
	}
	match := alterTargetPattern.FindStringSubmatch(sql)
	if match == nil {
		return fmt.Errorf("unsupported migration statement")
	}
	schema, name := foldIdent(match[1]), foldIdent(match[2])
	wantSchema := request.Table.Schema
	if wantSchema == "" {
		wantSchema = "public"
	}
	if name != request.Table.Name || (schema != "" && schema != wantSchema) {
		return fmt.Errorf("migration SQL targets %q but the request table "+
			"is %s.%s", strings.Trim(schema+"."+name, "."),
			wantSchema, request.Table.Name)
	}
	return nil
}

// topLevelCount counts sep outside parentheses, quotes and identifiers.
func topLevelCount(sql string, sep byte) int {
	depth, count := 0, 0
	var quote byte
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == sep && depth == 0:
			count++
		}
	}
	return count
}

func foldIdent(ident string) string {
	if len(ident) >= 2 && ident[0] == '"' {
		return strings.ReplaceAll(ident[1:len(ident)-1], `""`, `"`)
	}
	return strings.ToLower(ident)
}
