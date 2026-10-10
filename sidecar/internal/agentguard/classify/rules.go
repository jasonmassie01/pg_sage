package classify

import (
	"fmt"
	"strings"
	"unicode"
)

// rule proposes a class when a column name contains one of its phrases (a
// contiguous run of name tokens).
type rule struct {
	class     Class
	phrases   [][]string
	rationale string
	// textOnly limits the rule to text-like columns (free text).
	textOnly bool
}

func phrases(s ...string) [][]string {
	out := make([][]string, len(s))
	for i, p := range s {
		out[i] = strings.Fields(p)
	}
	return out
}

// rules, narrowest first: the first match wins.
var rules = []rule{
	{class: ClassSecret, rationale: "the name says it holds a credential or key",
		phrases: phrases("password", "passwd", "pwd", "secret", "api key", "apikey", "token",
			"private key", "signing key", "encryption key", "credential", "credentials",
			"card number", "cvv", "cvc", "otp", "totp")},
	{class: ClassPII, rationale: "the name says it identifies or contacts a person",
		phrases: phrases("email", "e mail", "emailaddress", "phone", "mobile", "telephone",
			"first name", "last name", "full name", "middle name", "given name",
			"family name", "surname", "firstname", "lastname", "fullname", "date of birth",
			"dob", "birthdate", "birth date", "birthday", "ssn", "social security",
			"national id", "passport", "tax id", "driver license", "drivers license",
			"street", "address", "postal code", "postcode", "zip", "zipcode", "ip",
			"iban", "account number")},
	{class: ClassUntrusted, textOnly: true,
		rationale: "the name says it holds free text that users or outsiders write",
		phrases: phrases("comment", "comments", "notes", "body", "message", "content",
			"prompt", "feedback", "review", "bio", "description", "user agent",
			"instructions")},
}

// metaSuffixes end names that describe a value rather than hold it:
// password_changed_at, comment_count.
var metaSuffixes = map[string]bool{"at": true, "on": true, "count": true, "changed": true,
	"updated": true, "expires": true, "expiry": true, "length": true, "len": true,
	"required": true, "policy": true, "enabled": true, "version": true, "attempts": true,
	"used": true, "id": true, "type": true, "kind": true, "status": true}

// Suggest proposes classes for cols from their names and types. It never
// proposes clean: only an operator says a column is clean.
func Suggest(cols []Column) []Proposal {
	var out []Proposal
	for _, c := range cols {
		if p, ok := suggestOne(c); ok {
			out = append(out, p)
		}
	}
	return out
}

func suggestOne(c Column) (Proposal, bool) {
	typ := strings.ToLower(c.Type)
	if c.Name == "" || typ == "boolean" {
		return Proposal{}, false
	}
	if isAddressType(typ) {
		return ruleProposal(c, ClassPII, "a network address identifies a person's device",
			"type "+c.Type), true
	}
	tokens := nameTokens(c.Name)
	if len(tokens) == 0 || metaSuffixes[tokens[len(tokens)-1]] {
		return Proposal{}, false
	}
	for _, r := range rules {
		if r.textOnly && !isTextType(typ) {
			continue
		}
		if r.class != ClassPII && isTemporalType(typ) {
			continue
		}
		for _, ph := range r.phrases {
			if containsRun(tokens, ph) {
				return ruleProposal(c, r.class, r.rationale,
					fmt.Sprintf("name %q matches %q; type %s", c.Name,
						strings.Join(ph, " "), c.Type)), true
			}
		}
	}
	return Proposal{}, false
}

func ruleProposal(c Column, class Class, rationale, detail string) Proposal {
	return Proposal{Column: c, Class: class, Source: SourceDetector,
		ProposedBy: RulesProposer, Rationale: rationale,
		Evidence: []Citation{{Kind: "column", Ref: c.QualifiedName(), Detail: detail}}}
}

// nameTokens splits an identifier on non-alphanumerics and camelCase.
func nameTokens(name string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(name)
	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			flush()
			continue
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(runes[i-1]):
			flush()
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

func containsRun(tokens, phrase []string) bool {
	for i := 0; i+len(phrase) <= len(tokens); i++ {
		match := true
		for j, p := range phrase {
			if tokens[i+j] != p {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func isTextType(t string) bool {
	for _, p := range []string{"text", "character", "varchar", "citext", "json", "xml",
		"char"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

func isTemporalType(t string) bool {
	return strings.HasPrefix(t, "timestamp") || strings.HasPrefix(t, "date") ||
		strings.HasPrefix(t, "time") || strings.HasPrefix(t, "interval")
}

func isAddressType(t string) bool { return t == "inet" || t == "cidr" || t == "macaddr" ||
	t == "macaddr8" }
