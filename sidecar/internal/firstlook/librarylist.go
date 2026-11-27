package firstlook

import (
	"slices"
	"strings"
)

// libraryList is the value of ALTER SYSTEM SET for a library list setting
// with lib added: one SQL literal per library, in the current order. One
// literal holding "a,b" names a single library called "a,b", so the server
// would refuse every new session (session_preload_libraries) or fail to
// start (shared_preload_libraries). A list that already names lib is kept
// as it is; an unreadable list keeps a placeholder the operator fills in.
func libraryList(list *string, lib string) string {
	libs := []string{"<existing libraries>"}
	if list != nil {
		libs = splitLibraries(*list)
	}
	if !slices.Contains(libs, lib) {
		libs = append(libs, lib)
	}
	quoted := make([]string, len(libs))
	for i, l := range libs {
		quoted[i] = "'" + strings.ReplaceAll(l, "'", "''") + "'"
	}
	return strings.Join(quoted, ", ")
}

// splitLibraries splits a list setting as pg_settings shows it: names are
// separated by commas, and a name in double quotes (with "" for a quote)
// may hold commas and spaces. Empty entries are dropped.
func splitLibraries(list string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(list); i++ {
		c := list[i]
		switch {
		case c == '"' && quoted && i+1 < len(list) && list[i+1] == '"':
			cur.WriteByte('"')
			i++
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = appendLibrary(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return appendLibrary(out, cur.String())
}

func appendLibrary(libs []string, name string) []string {
	if name = strings.TrimSpace(name); name != "" {
		return append(libs, name)
	}
	return libs
}
