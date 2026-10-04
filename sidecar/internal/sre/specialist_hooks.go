package sre

// Hooks for the Postgres-specialist contract (package specialist, roadmap
// phase 3): what pg_sage tells another agent is redacted with the replay
// export's rules. Secrets and PII-like literals are always removed;
// identifiers are kept or replaced by keyed hashes.

// ScrubRules names what Scrub removes, for a result's redaction header.
var ScrubRules = append(append([]string(nil), RedactionRules...), "token", "email",
	"phone", "card_or_id_number")

// Scrub removes credentials, connection URIs, bearer tokens, SQL literals,
// raw vectors, well-known token formats and PII-like literals from text.
// Identifiers and numbers are kept.
func Scrub(s string) string { return scrub(s) }

// IdentifierRedactor redacts free text and names with one key, the way a
// replay-case export does.
type IdentifierRedactor struct{ r redactor }

// NewIdentifierRedactor keeps identifiers (secrets and PII are still
// scrubbed) or hashes them with key; equal names hash equally under one key.
func NewIdentifierRedactor(keep bool, key []byte) IdentifierRedactor {
	return IdentifierRedactor{r: redactor{keep: keep, key: key}}
}

// Text redacts free text: secrets and PII always, quoted or
// schema-qualified names unless identifiers are kept.
func (i IdentifierRedactor) Text(s string) string { return i.r.text(s) }

// Name redacts one identifier: scrubbed when kept, otherwise its hash.
func (i IdentifierRedactor) Name(s string) string {
	if i.r.keep || s == "" {
		return scrub(s)
	}
	return i.r.token(s)
}
