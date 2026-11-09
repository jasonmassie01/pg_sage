package workload

import (
	"errors"
	"strings"
)

// errBadIdent means the text at the position is not an identifier.
var errBadIdent = errors.New("not an identifier")

// ReadIdent reads one identifier at s[i:]: a quoted one exactly (a
// doubled quote is one quote), a bare one folded to lower case. It returns
// the name and the index after it.
func ReadIdent(s string, i int) (string, int, error) {
	if i >= len(s) {
		return "", i, errBadIdent
	}
	if s[i] == '"' {
		var b strings.Builder
		for j := i + 1; j < len(s); j++ {
			if s[j] != '"' {
				b.WriteByte(s[j])
				continue
			}
			if j+1 < len(s) && s[j+1] == '"' {
				b.WriteByte('"')
				j++
				continue
			}
			if b.Len() == 0 {
				return "", j + 1, errBadIdent
			}
			return b.String(), j + 1, nil
		}
		return "", len(s), errBadIdent
	}
	j := i
	for j < len(s) && isIdentByte(s[j], j == i) {
		j++
	}
	if j == i {
		return "", i, errBadIdent
	}
	return strings.ToLower(s[i:j]), j, nil
}

func isIdentByte(c byte, first bool) bool {
	letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
	if first {
		return letter
	}
	return letter || c >= '0' && c <= '9' || c == '$'
}

// ReadQualified reads "name" or "schema.name" at s[i:] (spaces allowed
// around the dot); schema is empty for an unqualified name.
func ReadQualified(s string, i int) (schema, name string, next int, err error) {
	first, j, err := ReadIdent(s, i)
	if err != nil {
		return "", "", j, err
	}
	k := skipSpace(s, j)
	if k < len(s) && s[k] == '.' {
		second, l, err := ReadIdent(s, skipSpace(s, k+1))
		if err != nil {
			return "", "", l, err
		}
		return first, second, l, nil
	}
	return "", first, j, nil
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}
