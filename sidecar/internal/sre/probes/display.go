package probes

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// rowLimitParam is the row limit every catalog SQL takes as $1.
const rowLimitParam = "LIMIT $1"

var anyPositionalParam = regexp.MustCompile(`\$[0-9]`)

// DisplaySQL renders an argument-free catalog probe as standalone SQL an
// operator can run (a case's read-only diagnostic, playbooks-04): the
// lowest variant, which every supported server runs, with LIMIT $1
// replaced by the spec's MaxRows. It keeps the probe's identity comment,
// its current_database() scope and its row cap. It is display text only:
// the runner's read-only transaction, search_path and timeouts do not
// apply, so the caller states them. Probes that take arguments, read an
// extension through a run-time schema, or read pg_sage's history store
// are refused.
func DisplaySQL(id ID) (string, error) {
	s, ok := Catalog().Spec(id)
	switch {
	case !ok:
		return "", fmt.Errorf("probe %q is not in the catalog", id)
	case s.Args != ArgsNone || s.QueryScoped:
		return "", fmt.Errorf("probe %q takes arguments; only argument-free probes "+
			"render as display SQL", id)
	case s.Extension != "":
		return "", fmt.Errorf("probe %q reads extension %s through a schema resolved "+
			"at run time; it has no display SQL", id, s.Extension)
	case s.History:
		return "", fmt.Errorf("probe %q reads pg_sage's history store; it has no "+
			"display SQL", id)
	}
	sql := s.Variants[0].SQL
	if !strings.Contains(sql, rowLimitParam) {
		return "", fmt.Errorf("probe %q SQL has no %s row limit to inline", id, rowLimitParam)
	}
	out := strings.ReplaceAll(sql, rowLimitParam, "LIMIT "+strconv.Itoa(s.MaxRows))
	if anyPositionalParam.MatchString(out) {
		return "", fmt.Errorf("probe %q SQL keeps a positional parameter besides its "+
			"row limit", id)
	}
	return out, nil
}
