package agentposture

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// ap09ForeignSQL reads the remote wrappers, their servers and the user
// mappings on those servers, each with who holds USAGE (for a mapping:
// on its server) and, for a mapping, whose it is.
var ap09ForeignSQL = Statement("AP-09", `SELECT 'foreign_data_wrapper', w.fdwname::text,
  w.fdwname::text, `+grantees("COALESCE(w.fdwacl, pg_catalog.acldefault('F', w.fdwowner))",
	"USAGE")+`, NULL::oid
FROM pg_catalog.pg_foreign_data_wrapper w WHERE w.fdwname = ANY($1::text[])
UNION ALL
SELECT 'foreign_server', s.srvname::text, w.fdwname::text,
  `+grantees("COALESCE(s.srvacl, pg_catalog.acldefault('S', s.srvowner))", "USAGE")+`, NULL
FROM pg_catalog.pg_foreign_server s
JOIN pg_catalog.pg_foreign_data_wrapper w ON w.oid = s.srvfdw
WHERE w.fdwname = ANY($1::text[])
UNION ALL
SELECT 'user_mapping', s.srvname::text, w.fdwname::text,
  `+grantees("COALESCE(s.srvacl, pg_catalog.acldefault('S', s.srvowner))", "USAGE")+`,
  m.umuser
FROM pg_catalog.pg_user_mappings m
JOIN pg_catalog.pg_foreign_server s ON s.oid = m.srvid
JOIN pg_catalog.pg_foreign_data_wrapper w ON w.oid = s.srvfdw
WHERE w.fdwname = ANY($1::text[]) AND m.umuser = ANY($2::oid[])
ORDER BY 1, 2
LIMIT $3`)

type foreignRow struct {
	kind, name, wrapper string
	usage               []uint32
	mapUser             *uint32
}

// ap09Foreign reports remote wrappers and servers exposed roles can use,
// and user mappings (stored credentials) of exposed roles on such servers.
func ap09Foreign(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap09ForeignSQL, remoteWrappers, in.Env.ExposedOIDs(),
		apbMaxRows)
	if err != nil {
		return nil, fmt.Errorf("read foreign-data privileges: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var r foreignRow
		if err := rows.Scan(&r.kind, &r.name, &r.wrapper, &r.usage, &r.mapUser); err != nil {
			return nil, fmt.Errorf("read foreign-data privileges: %w", err)
		}
		if f, ok := foreignFinding(in.Env, r); ok {
			out = append(out, f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read foreign-data privileges: %w", err)
	}
	return out, nil
}

func foreignFinding(env Env, r foreignRow) (Finding, bool) {
	who := exposedHolders(env, r.usage)
	if len(who) == 0 {
		return Finding{}, false
	}
	if r.kind == "user_mapping" {
		return mappingFinding(env, r, who)
	}
	what := map[string]string{"foreign_data_wrapper": "FOREIGN DATA WRAPPER",
		"foreign_server": "FOREIGN SERVER"}[r.kind]
	var fix []string
	for _, g := range revokeTargets(who, r.usage) {
		fix = append(fix, "REVOKE USAGE ON "+what+" "+QuoteIdent(r.name)+" FROM "+
			revokeTarget(g)+";")
	}
	detail := roleText(who) + " can use " + strings.ToLower(what) + " " + r.name
	if r.kind == "foreign_data_wrapper" {
		detail += ": an agent can define servers and connect anywhere the server can reach."
	} else {
		detail += " (" + r.wrapper + "): an agent can define foreign tables over it and " +
			"connect with any user mapping that applies to it."
	}
	return Finding{Severity: Critical, ObjectType: r.kind, Object: r.name,
		Title:          "Remote-access " + strings.ToLower(what) + " usable by exposed roles",
		Detail:         detail,
		Recommendation: "Revoke USAGE from exposed roles; grant it only to the roles that need it.",
		FixScript:      strings.Join(fix, "\n"),
		Evidence: []Evidence{{Source: "aclexplode", Ref: r.kind + " " + r.name,
			Detail: "USAGE held by " + roleText(who)}}}, true
}

// mappingFinding reports a user mapping of an exposed role (or PUBLIC)
// on a remote server an exposed role can use: its stored credentials
// connect for whoever it maps.
func mappingFinding(env Env, r foreignRow, who []Role) (Finding, bool) {
	if r.mapUser == nil {
		return Finding{}, false
	}
	owner, ok := env.Exposure(*r.mapUser)
	if !ok || !(owner.OID == PublicOID || slices.ContainsFunc(who,
		func(x Role) bool { return x.OID == owner.OID })) {
		return Finding{}, false
	}
	user := revokeTarget(owner)
	return Finding{Severity: Critical, ObjectType: "user_mapping",
		Object: owner.Name + "@" + r.name,
		Title:  "Stored remote credentials usable by exposed roles",
		Detail: fmt.Sprintf("A user mapping for %s on server %s (%s) lets %s connect with "+
			"its stored credentials.", owner.Name, r.name, r.wrapper, roleText(who)),
		Recommendation: "Drop the mapping, or map only the roles that need the remote " +
			"server, with credentials limited to what they need there.",
		FixScript: "DROP USER MAPPING FOR " + user + " SERVER " + QuoteIdent(r.name) + ";",
		Evidence: []Evidence{{Source: "pg_user_mappings", Ref: owner.Name + "@" + r.name,
			Detail: "server USAGE held by " + roleText(who)}}}, true
}

// untrustedLanguages and untrustedHandlers name procedural languages that
// can reach the server's operating system; they must never be trusted.
var (
	untrustedLanguages = []string{"plperlu", "plpython3u", "plpythonu", "plpython2u",
		"pltclu", "plsh", "plr", "javau"}
	untrustedHandlers = []string{"plperlu_call_handler", "plpython3_call_handler",
		"plpython_call_handler", "plpython2_call_handler", "pltclu_call_handler",
		"plr_call_handler", "plsh_handler", "javau_call_handler"}
)

var ap09LanguageSQL = Statement("AP-09", `SELECT l.lanname::text, h.proname::text,
  `+grantees("COALESCE(l.lanacl, pg_catalog.acldefault('l', l.lanowner))", "USAGE")+`
FROM pg_catalog.pg_language l
JOIN pg_catalog.pg_proc h ON h.oid = l.lanplcallfoid
WHERE l.lanpltrusted AND (l.lanname = ANY($1::text[]) OR h.proname = ANY($2::text[]))
ORDER BY 1
LIMIT $3`)

// ap09Languages reports untrusted languages marked trusted that exposed
// roles may use: any function they create runs with the server's rights.
func ap09Languages(ctx context.Context, in Input) ([]Finding, error) {
	rows, err := in.Q.Query(ctx, ap09LanguageSQL, untrustedLanguages, untrustedHandlers,
		apbMaxRows)
	if err != nil {
		return nil, fmt.Errorf("read procedural languages: %w", err)
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var lang, handler string
		var usage []uint32
		if err := rows.Scan(&lang, &handler, &usage); err != nil {
			return nil, fmt.Errorf("read procedural languages: %w", err)
		}
		who := exposedHolders(in.Env, usage)
		if len(who) == 0 {
			continue
		}
		out = append(out, languageFinding(lang, handler, who, usage))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read procedural languages: %w", err)
	}
	return out, nil
}

func languageFinding(lang, handler string, who []Role, usage []uint32) Finding {
	var fix []string
	for _, g := range revokeTargets(who, usage) {
		fix = append(fix, "REVOKE USAGE ON LANGUAGE "+QuoteIdent(lang)+" FROM "+
			revokeTarget(g)+";")
	}
	fix = append(fix, "-- Untrusted languages must not be marked trusted. As a superuser:",
		"-- UPDATE pg_catalog.pg_language SET lanpltrusted = false WHERE lanname = '"+
			strings.ReplaceAll(lang, "'", "''")+"';")
	return Finding{Severity: Critical, ObjectType: "language", Object: lang,
		Title: "Untrusted language usable by exposed roles",
		Detail: fmt.Sprintf("Language %s (handler %s) can reach the operating system but is "+
			"marked trusted, so %s can create functions in it.", lang, handler, roleText(who)),
		Recommendation: "Revoke USAGE from exposed roles and mark the language untrusted " +
			"again; only superusers should create functions in it.",
		FixScript: strings.Join(fix, "\n"),
		Evidence: []Evidence{{Source: "pg_language", Ref: lang,
			Detail: "lanpltrusted = true, handler " + handler}}}
}
