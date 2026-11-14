//go:build cgo

package sqlast

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// market-08 role budgets reach the parse tree as ALTER ROLE <role> IN
// DATABASE <db> SET <timeout> and ALTER ROLE <role> CONNECTION LIMIT n.
// Both need an explicit allowlist: nil RoleParam refuses SET and nil
// RoleConnectionLimit refuses CONNECTION LIMIT (env-cgo).

// No state-transition tests: Check is stateless.

func roleRules() Rules {
	rules := testRules
	rules.RoleParam = func(name string) bool {
		return name == "statement_timeout" || name == "idle_in_transaction_session_timeout"
	}
	rules.RoleConnectionLimit = func(role string) bool { return role == "app" }
	return rules
}

func TestCheckAcceptsRoleBudgetStatements(t *testing.T) {
	for _, sql := range []string{
		"ALTER ROLE app IN DATABASE db SET statement_timeout = '30s'",
		"ALTER ROLE app IN DATABASE db SET idle_in_transaction_session_timeout TO 60000",
		"ALTER ROLE app IN DATABASE db RESET statement_timeout",
		`ALTER ROLE "app" IN DATABASE "db" SET statement_timeout = '30s'`,
		"ALTER ROLE app CONNECTION LIMIT 20",
		"ALTER ROLE app WITH CONNECTION LIMIT 20",
		`ALTER ROLE "app" CONNECTION LIMIT 20`,
		"ALTER USER app CONNECTION LIMIT 20",
	} {
		if err := Check(sql, roleRules()); err != nil {
			t.Errorf("Check(%q) = %v, want accepted", sql, err)
		}
	}
}

func TestCheckRejectsRoleStatements(t *testing.T) {
	setCases := map[string]string{
		"ALTER ROLE ALL SET statement_timeout = '1s'":                 "one role",
		"ALTER ROLE ALL IN DATABASE db SET statement_timeout = '1s'":  "one role",
		"ALTER ROLE CURRENT_USER IN DATABASE db SET work_mem = '1MB'": "one role",
		"ALTER ROLE SESSION_USER IN DATABASE db SET work_mem = '1MB'": "one role",
		"ALTER ROLE app SET statement_timeout = '30s'":                "IN DATABASE",
		"ALTER ROLE app IN DATABASE db SET search_path = public":      "search_path",
		"ALTER ROLE app IN DATABASE db RESET ALL":                     "not allowed",
		"ALTER ROLE app IN DATABASE db SET statement_timeout =":       "parse error",
	}
	limitCases := map[string]string{
		"ALTER ROLE CURRENT_USER CONNECTION LIMIT 5":     "one role",
		"ALTER ROLE app SUPERUSER":                       "only the connection limit",
		"ALTER ROLE app CONNECTION LIMIT 5 CREATEDB":     "only the connection limit",
		"ALTER ROLE app NOLOGIN CONNECTION LIMIT 0":      "only the connection limit",
		"ALTER ROLE app PASSWORD 'x'":                    "only the connection limit",
		"ALTER ROLE postgres CONNECTION LIMIT 5":         "postgres",
		`ALTER ROLE U&"\0070ostgres" CONNECTION LIMIT 5`: "postgres",
		`ALTER ROLE "App" CONNECTION LIMIT 5`:            "App",
		"ALTER ROLE app RENAME TO app2":                  "statement kind",
		"ALTER ROLE app CONNECTION LIMIT":                "parse error",
	}
	for _, cases := range []map[string]string{setCases, limitCases} {
		for sql, want := range cases {
			err := Check(sql, roleRules())
			if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), want) {
				t.Errorf("Check(%q) = %v, want ErrRejected mentioning %q", sql, err, want)
			}
		}
	}
}

// The allowlist sees the role as PostgreSQL resolves it: a quoted,
// Unicode-escaped spelling cannot slip past a name check.
func TestCheckRoleConnectionLimitSeesDecodedRole(t *testing.T) {
	var seen []string
	rules := roleRules()
	rules.RoleConnectionLimit = func(role string) bool {
		seen = append(seen, role)
		return false
	}
	err := Check(`ALTER ROLE U&"\0070ostgres" CONNECTION LIMIT 5`, rules)
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("Check = %v, want ErrRejected", err)
	}
	if len(seen) != 1 || seen[0] != "postgres" {
		t.Fatalf("RoleConnectionLimit saw %q, want [postgres]", seen)
	}
}

func TestCheckRoleStatementsNeedAnAllowlist(t *testing.T) {
	cases := map[string]string{
		"ALTER ROLE app IN DATABASE db SET statement_timeout = '30s'": "ALTER ROLE ... SET",
		"ALTER ROLE app CONNECTION LIMIT 5":                           "CONNECTION LIMIT",
		"ALTER ROLE x CONNECTION LIMIT 5":                             "CONNECTION LIMIT",
		"ALTER ROLE app CONNECTION LIMIT 0":                           "CONNECTION LIMIT",
		"ALTER ROLE app CONNECTION LIMIT -1":                          "CONNECTION LIMIT",
	}
	for name, rules := range map[string]Rules{"testRules": testRules, "zero Rules": {}} {
		for sql, want := range cases {
			err := Check(sql, rules)
			if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: Check(%q) = %v, want ErrRejected mentioning %q",
					name, sql, err, want)
			}
		}
	}
}

func TestCheckRoleConnectionLimitBoundaries(t *testing.T) {
	for _, n := range []string{"-1", "0", "1", "2147483647"} {
		allowed := "ALTER ROLE app CONNECTION LIMIT " + n
		if err := Check(allowed, roleRules()); err != nil {
			t.Errorf("Check(%q) = %v, want accepted", allowed, err)
		}
		refused := "ALTER ROLE postgres CONNECTION LIMIT " + n
		if err := Check(refused, roleRules()); !errors.Is(err, ErrRejected) {
			t.Errorf("Check(%q) = %v, want ErrRejected", refused, err)
		}
	}
	for _, sql := range []string{
		"", "   ", "-- ALTER ROLE app CONNECTION LIMIT 5",
		"ALTER ROLE app CONNECTION LIMIT 5; ALTER ROLE postgres CONNECTION LIMIT 5",
		"ALTER ROLE app CONNECTION LIMIT 5; ALTER ROLE app CONNECTION LIMIT 6",
		"ALTER ROLE app IN DATABASE db SET statement_timeout = '1s'; " +
			"ALTER ROLE app IN DATABASE db SET search_path = public",
	} {
		if err := Check(sql, roleRules()); !errors.Is(err, ErrRejected) {
			t.Errorf("Check(%q) = %v, want ErrRejected", sql, err)
		}
	}
}

// pg_query_go parses in C; concurrent callers must each get their own
// verdict. Run under -race in the Linux cgo container.
func TestCheckRoleStatementsConcurrently(t *testing.T) {
	statements := []string{
		"ALTER ROLE app CONNECTION LIMIT 20",
		"ALTER ROLE postgres CONNECTION LIMIT 20",
		"ALTER ROLE app IN DATABASE db SET statement_timeout = '30s'",
		"ALTER ROLE app IN DATABASE db SET search_path = public",
		"ALTER ROLE app SUPERUSER",
	}
	want := make([]bool, len(statements))
	for i, sql := range statements {
		want[i] = Check(sql, roleRules()) == nil
	}
	if !want[0] || want[1] || !want[2] || want[3] || want[4] {
		t.Fatalf("sequential verdicts = %v, want [true false true false false]", want)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 32*len(statements))
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, sql := range statements {
				if got := Check(sql, roleRules()) == nil; got != want[i] {
					errs <- sql
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for sql := range errs {
		t.Errorf("concurrent Check(%q) disagreed with the sequential verdict", sql)
	}
}
