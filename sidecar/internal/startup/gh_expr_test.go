package startup

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
)

// ci-05: a small evaluator for the GitHub Actions expressions the release
// gates use (&&, ||, !, ==, !=, parentheses, string literals, contexts,
// always(), success(), failure(), cancelled(), startsWith() and contains()), so the
// release tests can ask "does this job run when lint failed on a tag?"
// instead of matching substrings of the if.

// ghState is one hypothetical run: its contexts and the results of the
// job's needs.
type ghState struct {
	ctx   map[string]string // github.ref, needs.lint.result, ...
	needs []string
}

type ghParser struct {
	toks []string
	pos  int
	st   ghState
}

// ghIf reports whether a job with this if (empty for none) runs. Without a
// status function GitHub adds an implicit success(): every need succeeded.
func ghIf(cond string, st ghState) (bool, error) {
	cond = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(
		strings.TrimSpace(cond), "${{"), "}}"))
	if cond == "" {
		cond = "success()"
	}
	if !strings.Contains(cond, "always()") && !strings.Contains(cond, "success()") &&
		!strings.Contains(cond, "failure()") && !strings.Contains(cond, "cancelled()") {
		cond = "success() && (" + cond + ")"
	}
	toks, err := ghTokens(cond)
	if err != nil {
		return false, err
	}
	p := &ghParser{toks: toks, st: st}
	v, err := p.or()
	if err != nil {
		return false, err
	}
	if p.pos != len(p.toks) {
		return false, fmt.Errorf("trailing %q in %q", p.toks[p.pos], cond)
	}
	return truthy(v), nil
}

func ghTokens(s string) ([]string, error) {
	var out []string
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case strings.HasPrefix(s[i:], "&&"), strings.HasPrefix(s[i:], "||"),
			strings.HasPrefix(s[i:], "=="), strings.HasPrefix(s[i:], "!="):
			out, i = append(out, s[i:i+2]), i+2
		case strings.ContainsRune("()!,", c):
			out, i = append(out, string(c)), i+1
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated string in %q", s)
			}
			out, i = append(out, s[i:i+end+2]), i+end+2
		case unicode.IsLetter(c):
			j := i
			for j < len(s) && (unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])) ||
				strings.ContainsRune("._-", rune(s[j]))) {
				j++
			}
			out, i = append(out, s[i:j]), j
		default:
			return nil, fmt.Errorf("unexpected %q in %q", c, s)
		}
	}
	return out, nil
}

func (p *ghParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *ghParser) next() string { t := p.peek(); p.pos++; return t }

func (p *ghParser) expect(tok string) error {
	if got := p.next(); got != tok {
		return fmt.Errorf("want %q, got %q at token %d", tok, got, p.pos-1)
	}
	return nil
}

func (p *ghParser) or() (any, error)  { return p.binary("||", p.and) }
func (p *ghParser) and() (any, error) { return p.binary("&&", p.unary) }

func (p *ghParser) binary(op string, operand func() (any, error)) (any, error) {
	left, err := operand()
	if err != nil {
		return nil, err
	}
	for p.peek() == op {
		p.next()
		right, err := operand()
		if err != nil {
			return nil, err
		}
		if op == "&&" {
			left = truthy(left) && truthy(right)
		} else {
			left = truthy(left) || truthy(right)
		}
	}
	return left, nil
}

func (p *ghParser) unary() (any, error) {
	if p.peek() == "!" {
		p.next()
		v, err := p.unary()
		return !truthy(v), err
	}
	left, err := p.primary()
	if err != nil {
		return nil, err
	}
	if op := p.peek(); op == "==" || op == "!=" {
		p.next()
		right, err := p.primary()
		if err != nil {
			return nil, err
		}
		eq := fmt.Sprint(left) == fmt.Sprint(right)
		return eq == (op == "=="), nil
	}
	return left, nil
}

func (p *ghParser) primary() (any, error) {
	tok := p.next()
	switch {
	case tok == "(":
		v, err := p.or()
		if err != nil {
			return nil, err
		}
		return v, p.expect(")")
	case strings.HasPrefix(tok, "'"):
		return strings.Trim(tok, "'"), nil
	case tok == "true" || tok == "false":
		return tok == "true", nil
	case p.peek() == "(":
		return p.call(tok)
	case tok != "" && unicode.IsLetter(rune(tok[0])):
		return p.st.ctx[tok], nil
	}
	return nil, fmt.Errorf("unexpected token %q", tok)
}

func (p *ghParser) call(fn string) (any, error) {
	p.next()
	var args []any
	for p.peek() != ")" {
		v, err := p.or()
		if err != nil {
			return nil, err
		}
		args = append(args, v)
		if p.peek() == "," {
			p.next()
		}
	}
	p.next()
	switch fn {
	case "always":
		return true, nil
	case "cancelled":
		return false, nil
	case "success", "failure":
		return p.st.allNeedsSucceeded() == (fn == "success"), nil
	case "startsWith":
		if len(args) != 2 {
			return nil, fmt.Errorf("startsWith takes 2 arguments, got %d", len(args))
		}
		return strings.HasPrefix(fmt.Sprint(args[0]), fmt.Sprint(args[1])), nil
	case "contains":
		if len(args) != 2 {
			return nil, fmt.Errorf("contains takes 2 arguments, got %d", len(args))
		}
		// GitHub compares strings without regard to case.
		return strings.Contains(strings.ToLower(fmt.Sprint(args[0])),
			strings.ToLower(fmt.Sprint(args[1]))), nil
	}
	return nil, fmt.Errorf("unknown function %s()", fn)
}

func (st ghState) allNeedsSucceeded() bool {
	for _, n := range st.needs {
		if st.ctx["needs."+n+".result"] != "success" {
			return false
		}
	}
	return true
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != ""
	}
	return false
}

// pushState is a push of ref where every need succeeded.
func pushState(ref string, needs []string) ghState {
	ctx := map[string]string{"github.event_name": "push", "github.ref": ref}
	for _, n := range needs {
		ctx["needs."+n+".result"] = "success"
	}
	return ghState{ctx: ctx, needs: needs}
}

// with returns a copy of st with one need's result changed.
func (st ghState) with(need, result string) ghState {
	ctx := make(map[string]string, len(st.ctx))
	for k, v := range st.ctx {
		ctx[k] = v
	}
	ctx["needs."+need+".result"] = result
	return ghState{ctx: ctx, needs: st.needs}
}

// No concurrent-access tests: the evaluator is pure and shares nothing.

func TestGHIfEvaluatesGateExpressions(t *testing.T) {
	needs := []string{"test", "lint"}
	tag := pushState("refs/tags/v2.6.0", needs)
	cases := []struct {
		name, cond string
		st         ghState
		want       bool
	}{
		{"no if, all succeeded", "", tag, true},
		{"no if, a need failed", "", tag.with("lint", "failure"), false},
		{"implicit success() on a plain if", "startsWith(github.ref, 'refs/tags/v')",
			tag.with("test", "skipped"), false},
		{"always() ignores results", "always()", tag.with("test", "failure"), true},
		{"explicit result check", "always() && needs.lint.result == 'success'",
			tag.with("lint", "failure"), false},
		{"or of ref and result", "always() && (github.ref == 'refs/heads/master' || " +
			"needs.lint.result == 'success')", tag.with("lint", "failure"), false},
		{"master passes the or", "always() && (github.ref == 'refs/heads/master' || " +
			"needs.lint.result == 'success')",
			pushState("refs/heads/master", needs).with("lint", "failure"), true},
		{"negated cancelled", "${{ !cancelled() && needs.test.result != 'failure' }}",
			tag, true},
		{"not equal", "always() && github.event_name != 'push'", tag, false},
	}
	for _, c := range cases {
		got, err := ghIf(c.cond, c.st)
		if err != nil {
			t.Fatalf("%s: ghIf(%q): %v", c.name, c.cond, err)
		}
		if got != c.want {
			t.Errorf("%s: ghIf(%q) = %v, want %v", c.name, c.cond, got, c.want)
		}
	}
}

func TestGHIfRejectsMalformedExpressions(t *testing.T) {
	st := pushState("refs/tags/v1.0.0", nil)
	for _, cond := range []string{
		"always() && (github.ref == 'x'",        // unclosed paren
		"always() && github.ref == 'x",          // unterminated string
		"always() && startsWith('a')",           // wrong arity
		"always() && contains(github.ref)",      // wrong arity
		"always() && endsWith(github.ref, 'x')", // unsupported function
		"always() && github.ref == 'x' )",       // trailing token
		"always() && github.ref ~ 'x'",          // unknown operator
	} {
		if got, err := ghIf(cond, st); err == nil {
			t.Errorf("ghIf(%q) = %v, want a parse error", cond, got)
		}
	}
}

func TestGHIfContains(t *testing.T) {
	cases := []struct {
		cond, ref string
		want      bool
	}{
		{"contains(github.ref, '-')", "refs/tags/v2.6.0-rc1", true},
		{"contains(github.ref, '-')", "refs/tags/v2.6.0", false},
		{"contains(github.ref, 'RC')", "refs/tags/v2.6.0-rc1", true},
		{"!contains(github.ref, '-')", "refs/tags/v2.6.0", true},
		{"!contains(github.ref, '-')", "refs/tags/v2.6.0-rc1", false},
		{"contains(github.ref, '')", "refs/tags/v2.6.0", true},
		{"contains(github.missing, '-')", "refs/tags/v2.6.0-rc1", false},
	}
	for _, c := range cases {
		got, err := ghIf(c.cond, pushState(c.ref, nil))
		if err != nil {
			t.Fatalf("ghIf(%q) on %s: %v", c.cond, c.ref, err)
		}
		if got != c.want {
			t.Errorf("ghIf(%q) on %s = %v, want %v", c.cond, c.ref, got, c.want)
		}
	}
}
