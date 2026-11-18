package workpack

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/workload"
)

// A workload family groups the statements that do the same thing to the
// same tables: the statement's command plus its referenced relations
// (workload.ReferencedTables against the window's last tables snapshot),
// sorted. A statement with no text or no command is its own family,
// "unparsed:<queryid>".

// familyKey is queryid's family for the statement text.
func familyKey(qid int64, text string, known map[string][]string) string {
	key, _, _ := familyParts(qid, text, known)
	return key
}

// familyParts is the family key, its command and its relations
// ("schema.name", sorted; empty, never nil, when there are none).
func familyParts(qid int64, text string, known map[string][]string) (string, string,
	[]string) {
	cmd := command(text)
	if cmd == "" {
		return "unparsed:" + itoa64(qid), "", []string{}
	}
	refs := workload.ReferencedTables(text, known)
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Schema + "." + r.Name
	}
	if len(names) == 0 {
		return cmd, cmd, names
	}
	return cmd + " " + strings.Join(names, ","), cmd, names
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// mainCommands end a WITH clause: the statement the CTEs serve.
var mainCommands = map[string]bool{"select": true, "insert": true, "update": true,
	"delete": true, "merge": true, "values": true, "table": true}

// command is the statement's command, lower case: its first word, or for
// WITH the first main command at the WITH's parenthesis depth. Literals
// and comments are never read. "" when there is none.
func command(text string) string {
	blank := workload.BlankLiteralsAndComments(text)
	depth, withDepth, inWith := 0, 0, false
	for i := 0; i < len(blank); {
		c := blank[i]
		switch {
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			i++
		case isWordByte(c):
			j := i
			for j < len(blank) && (isWordByte(blank[j]) || blank[j] >= '0' && blank[j] <= '9') {
				j++
			}
			word := strings.ToLower(blank[i:j])
			i = j
			switch {
			case inWith && depth == withDepth && mainCommands[word]:
				return word
			case inWith:
			case word == "with":
				inWith, withDepth = true, depth
			default:
				return word
			}
		default:
			i++
		}
	}
	return ""
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// familyID is the family's evidence id: 12 hex of the key's SHA-256.
func familyID(key string) string {
	return "workload/family/" + familyHex(key)
}

func familyHex(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// queryEvidenceID is a queryid's evidence id.
func queryEvidenceID(qid int64) string {
	return "workload/query/" + itoa64(qid)
}
