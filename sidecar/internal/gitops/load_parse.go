package gitops

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/review/rubric"
)

// mapping is a YAML mapping's values by key, with each key's line.
type mapping struct {
	src    Source
	line   int
	order  []string
	values map[string]*yaml.Node
}

func newMapping(f rawFile, n *yaml.Node) mapping {
	m := mapping{src: Source{Path: f.path, Line: n.Line, Hash: f.hash,
		lines: map[string]int{}}, line: n.Line, values: map[string]*yaml.Node{}}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		m.order = append(m.order, key)
		m.values[key] = n.Content[i+1]
		m.src.lines[key] = n.Content[i].Line
	}
	return m
}

func (m mapping) problem(key, format string, args ...any) Problem {
	return m.src.problem(key, format, args...)
}

// at is a problem at the mapping's own line (a grant entry's).
func (m mapping) at(_ string, format string, args ...any) Problem {
	return Problem{File: m.src.Path, Line: m.line, Message: fmt.Sprintf(format, args...)}
}

// str is a scalar value as text.
func (m mapping) str(key string) (string, bool) {
	n, ok := m.values[key]
	if !ok || n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
}

func (m mapping) integer(key string) (int, bool) {
	s, ok := m.str(key)
	if !ok {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	return v, err == nil
}

// flag is a boolean key; absent is false. ok is false for a non-boolean.
func (m mapping) flag(key string) (value, ok bool) {
	n, present := m.values[key]
	if !present {
		return false, true
	}
	var b bool
	if err := n.Decode(&b); err != nil {
		return false, false
	}
	return b, true
}

// unknownKeys reports every key outside allowed.
func (m mapping) unknownKeys(allowed ...string) []Problem {
	ok := map[string]bool{"kind": true, "schema_version": true}
	for _, k := range allowed {
		ok[k] = true
	}
	var out []Problem
	for _, k := range m.order {
		if !ok[k] {
			out = append(out, m.problem(k, "unknown key %q", k))
		}
	}
	return out
}

func rubricDimensions() []string {
	dims := rubric.V1().Dimensions
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		out = append(out, d.ID)
	}
	return out
}

// parseSet decodes a set file with fleetpol-01a's parser; kind,
// schema_version and adopt are this wrapper's own keys.
func (l *loader) parseSet(m mapping) {
	adopt, ok := m.flag("adopt")
	if !ok {
		l.add(m.problem("adopt", "adopt must be true or false"))
		return
	}
	tree := map[string]any{}
	for _, k := range m.order {
		if k == "kind" || k == "schema_version" || k == "adopt" {
			continue
		}
		var v any
		if err := m.values[k].Decode(&v); err != nil {
			l.add(m.problem(k, "%s: %v", k, err))
			return
		}
		tree[k] = v
	}
	file, err := policy.DecodeSetTree(tree, rubricDimensions())
	if err != nil {
		l.add(m.problem(setErrorKey(m, err), "%v", err))
		return
	}
	if file.Document != nil {
		if _, err := policy.ParseDocument(file.Document.Body); err != nil {
			l.add(m.problem("document", "document: %v", err))
			return
		}
	}
	src := m.src
	src.Line = src.lineOf("name")
	if l.claim(l.sets, file.Name, src, "policy set "+file.Name) {
		l.dir.Sets = append(l.dir.Sets, SetSpec{Source: src, File: file, Adopt: adopt})
	}
}

// setErrorKey is the key a set error is about: the first selector that
// does not decode alone, else a key the message names, else the file.
func setErrorKey(m mapping, err error) string {
	if n := m.values["selectors"]; n != nil && n.Kind == yaml.SequenceNode {
		for i, item := range n.Content {
			if !selectorDecodes(item) {
				key := fmt.Sprintf("selectors[%d]", i)
				m.src.lines[key] = item.Line
				return key
			}
		}
	}
	for _, k := range m.order {
		if strings.Contains(err.Error(), k) {
			return k
		}
	}
	return "kind"
}

// selectorDecodes reports whether one selector item decodes strictly.
func selectorDecodes(item *yaml.Node) bool {
	var v any
	if err := item.Decode(&v); err != nil {
		return false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var sel policy.Selector
	return policy.DecodeStrictJSON(raw, &sel) == nil
}

// parsePrincipal reads a principal file and checks it as Create would.
func (l *loader) parsePrincipal(m mapping) {
	before := len(l.problems)
	l.problems = append(l.problems, m.unknownKeys("name", "tenant", "profile",
		"env_ceiling", "sponsor", "adopt")...)
	name, _ := m.str("name")
	tenant, _ := m.str("tenant")
	profile, _ := m.str("profile")
	ceiling, _ := m.str("env_ceiling")
	sponsor, _ := m.str("sponsor")
	adopt, ok := m.flag("adopt")
	if !ok {
		l.add(m.problem("adopt", "adopt must be true or false"))
	}
	if ceiling == "" {
		ceiling = string(agentguard.EnvDev)
	}
	checks := []struct {
		key string
		err error
	}{
		{"name", agentguard.ValidateCreate(name, "", agentguard.LegacyProfile, "")},
		{"tenant", agentguard.ValidateCreate("x-agent", tenant, agentguard.LegacyProfile, "")},
		{"profile", agentguard.ValidateCreate("x-agent", "", profile, "")},
		{"env_ceiling", agentguard.ValidateCreate("x-agent", "", agentguard.LegacyProfile,
			agentguard.Env(ceiling))},
	}
	for _, c := range checks {
		if c.err != nil {
			l.add(m.problem(c.key, "%s: %v", c.key, c.err))
		}
	}
	if len(l.problems) > before {
		return
	}
	src := m.src
	src.Line = src.lineOf("name")
	if l.claim(l.agents, name, src, "principal "+name) {
		l.dir.Principals = append(l.dir.Principals, PrincipalSpec{Source: src, Name: name,
			Tenant: tenant, Profile: profile, EnvCeiling: agentguard.Env(ceiling),
			Sponsor: strings.ToLower(strings.TrimSpace(sponsor)), Adopt: adopt})
	}
}

// parseGrants reads a grants file's list.
func (l *loader) parseGrants(m mapping) {
	l.problems = append(l.problems, m.unknownKeys("grants")...)
	list, ok := m.values["grants"]
	if !ok || list.Kind != yaml.SequenceNode {
		l.add(m.problem("grants", "grants: must be a list of user, database and role"))
		return
	}
	for _, item := range list.Content {
		if item.Kind != yaml.MappingNode {
			l.add(Problem{File: m.src.Path, Line: item.Line,
				Message: "each grant is a mapping of user, database and role"})
			continue
		}
		l.parseGrant(newMapping(rawFile{path: m.src.Path, hash: m.src.Hash}, item))
	}
}

func (l *loader) parseGrant(g mapping) {
	before := len(l.problems)
	for _, p := range g.unknownKeys("user", "database", "role", "adopt") {
		l.add(Problem{File: p.File, Line: g.line, Message: p.Message})
	}
	user, _ := g.str("user")
	database, _ := g.str("database")
	role, _ := g.str("role")
	adopt, ok := g.flag("adopt")
	user = strings.ToLower(strings.TrimSpace(user))
	switch {
	case !ok:
		l.add(g.at("adopt", "adopt must be true or false"))
	case user == "" || !strings.Contains(user, "@"):
		l.add(g.at("user", "user must be a pg_sage user's email"))
	case database == "" || database == "all" || database == "*":
		l.add(g.at("database", "database must name one monitored database"))
	case !auth.IsGrantRole(role):
		l.add(g.at("role", "role must be viewer or operator, got %q", role))
	}
	if len(l.problems) > before {
		return
	}
	src := g.src
	if l.claim(l.grants, user+"|"+database, src, "the grant of "+user+" on "+database) {
		l.dir.Grants = append(l.dir.Grants, GrantSpec{Source: src, User: user,
			Database: database, Role: role, Adopt: adopt})
	}
}
