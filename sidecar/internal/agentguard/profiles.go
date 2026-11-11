package agentguard

import (
	"sort"
	"strings"
)

// Built-in agent profiles (spec §9 agents.profiles). pg_sage ships them
// fixed; this table is the one source: decide.DefaultProfiles is built
// from it, so the gate's D4/D3 and the store's widening check (agentgov-06)
// judge the same grants. Classes are decide's capability class names.

// ProfileGrant is what a profile allows: its capability classes, its
// environment ceiling (the effective ceiling is the narrower of this and
// the principal's) and whether its writes may take the direct lane.
type ProfileGrant struct {
	Classes    []string
	EnvCeiling Env
	DirectLane bool
}

// ProfileInfo is one built-in profile as the API lists it.
type ProfileInfo struct {
	Name       string   `json:"name"`
	Classes    []string `json:"classes"`
	EnvCeiling Env      `json:"env_ceiling"`
	DirectLane bool     `json:"direct_lane"`
}

func builtInProfiles() map[string]ProfileGrant {
	return map[string]ProfileGrant{
		"readonly-analyst": {Classes: []string{"read"}, EnvCeiling: EnvProd},
		"app-writer": {Classes: []string{"read", "write_insert", "write_update"},
			EnvCeiling: EnvProd, DirectLane: true},
		"coding-agent": {Classes: []string{"read", "write_insert", "write_update",
			"write_delete", "ddl_additive", "ddl_locking", "sandbox"},
			EnvCeiling: EnvStage},
		LegacyProfile: {Classes: []string{"read"}, EnvCeiling: EnvProd},
	}
}

// BuiltInProfile returns the named built-in profile.
func BuiltInProfile(name string) (ProfileGrant, bool) {
	g, ok := builtInProfiles()[name]
	return g, ok
}

// KnownProfile reports whether name is a built-in profile.
func KnownProfile(name string) bool {
	_, ok := builtInProfiles()[name]
	return ok
}

// ProfileNames are the built-in profiles, sorted.
func ProfileNames() []string {
	all := builtInProfiles()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ProfileCatalog lists the built-in profiles by name; each call returns
// fresh slices.
func ProfileCatalog() []ProfileInfo {
	all := builtInProfiles()
	out := make([]ProfileInfo, 0, len(all))
	for _, n := range ProfileNames() {
		g := all[n]
		out = append(out, ProfileInfo{Name: n, Classes: g.Classes, EnvCeiling: g.EnvCeiling,
			DirectLane: g.DirectLane})
	}
	return out
}

// unknownProfile is the refusal of a profile outside the table.
func unknownProfile(name string) error {
	return invalid("profile %q is not a known profile; use one of: %s", name,
		strings.Join(ProfileNames(), ", "))
}

// profileWidens reports whether moving a principal from profile from at
// ceiling fromCeil to profile to at toCeil lets it do more. An unknown
// profile on either side counts as widening: an unknown one allowed
// nothing, and a move to one is never assumed safe.
func profileWidens(from string, fromCeil Env, to string, toCeil Env) bool {
	cur, okCur := BuiltInProfile(from)
	next, okNext := BuiltInProfile(to)
	if !okCur || !okNext {
		return from != to
	}
	return grantWidens(cur, fromCeil, next, toCeil)
}

// grantWidens: next widens cur when it adds a class, reaches a higher
// effective ceiling or gains the direct lane.
func grantWidens(cur ProfileGrant, curCeil Env, next ProfileGrant, nextCeil Env) bool {
	have := make(map[string]bool, len(cur.Classes))
	for _, c := range cur.Classes {
		have[c] = true
	}
	for _, c := range next.Classes {
		if !have[c] {
			return true
		}
	}
	if MinEnv(nextCeil, next.EnvCeiling).Rank() > MinEnv(curCeil, cur.EnvCeiling).Rank() {
		return true
	}
	return next.DirectLane && !cur.DirectLane
}
