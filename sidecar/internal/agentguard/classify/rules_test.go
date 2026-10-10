package classify

import (
	"testing"
)

// Spec §6.16: classification proposals from names and types, with name
// heuristics as the deterministic fallback. Rules never propose clean.

func ruleCol(name, typ string) Column {
	return Column{RelID: 20, AttNum: 1, Schema: "app", Table: "t", Name: name, Type: typ}
}

func TestSuggestRules(t *testing.T) {
	cases := []struct {
		name, typ string
		want      Class
	}{
		{"password", "text", ClassSecret},
		{"password_hash", "text", ClassSecret},
		{"PasswordDigest", "text", ClassSecret},
		{"api_key", "text", ClassSecret},
		{"apiKey", "varchar", ClassSecret},
		{"refresh_token", "text", ClassSecret},
		{"client_secret", "text", ClassSecret},
		{"private_key", "bytea", ClassSecret},
		{"card_number", "text", ClassSecret},
		{"cvv", "text", ClassSecret},
		{"email", "text", ClassPII},
		{"contact_email_address", "citext", ClassPII},
		{"phone_number", "text", ClassPII},
		{"first_name", "text", ClassPII},
		{"lastName", "text", ClassPII},
		{"date_of_birth", "date", ClassPII},
		{"ssn", "text", ClassPII},
		{"passport_no", "text", ClassPII},
		{"street_address", "text", ClassPII},
		{"postal_code", "text", ClassPII},
		{"client_ip", "inet", ClassPII},
		{"last_seen_from", "inet", ClassPII},
		{"comment", "text", ClassUntrusted},
		{"message_body", "text", ClassUntrusted},
		{"prompt", "jsonb", ClassUntrusted},
		{"user_agent", "text", ClassUntrusted},
		{"id", "bigint", Unclassified},
		{"created_at", "timestamptz", Unclassified},
		{"status", "text", Unclassified},
		{"name", "text", Unclassified},           // too broad: product names
		{"tokens_used", "integer", Unclassified}, // a count, not a token
		{"comment_count", "integer", Unclassified},
		{"password_changed_at", "timestamptz", Unclassified},
		{"description", "integer", Unclassified}, // free-text names need a text type
		{"", "text", Unclassified},
	}
	for _, c := range cases {
		got := Suggest([]Column{ruleCol(c.name, c.typ)})
		if c.want == Unclassified {
			if len(got) != 0 {
				t.Errorf("%s %s: suggested %s, want nothing", c.name, c.typ, got[0].Class)
			}
			continue
		}
		if len(got) != 1 || got[0].Class != c.want {
			t.Errorf("%s %s: %+v, want %s", c.name, c.typ, got, c.want)
			continue
		}
		p := got[0]
		if p.Source != SourceDetector || p.ProposedBy != RulesProposer ||
			len(p.Evidence) == 0 || p.Evidence[0].Ref != "app.t."+c.name || p.Rationale == "" {
			t.Errorf("%s: proposal provenance %+v", c.name, p)
		}
	}
}

func TestSuggestRulesNeverProposeClean(t *testing.T) {
	cols := []Column{ruleCol("id", "uuid"), ruleCol("password", "text"),
		ruleCol("total", "numeric")}
	for _, p := range Suggest(cols) {
		if p.Class == ClassClean {
			t.Fatalf("rules proposed clean for %s", p.Column.Name)
		}
	}
	if got := Suggest(nil); len(got) != 0 {
		t.Fatalf("nil columns: %v", got)
	}
}
