package auditchain

import (
	"fmt"
	"strings"
)

// lockClass is the first key of the advisory locks that order chain
// writers ('pgsa'); the second is hashtext(chain).
const lockClass = 1885827937

// ddlLinkTable is the shared link table and the install metadata.
const ddlLinkTable = `
CREATE TABLE IF NOT EXISTS sage.audit_chain_link (
    chain       text        NOT NULL,
    seq         bigint      NOT NULL,
    row_id      bigint      NOT NULL,
    op          text        NOT NULL CHECK (op IN ('I','U','D','T')),
    v           integer     NOT NULL,
    sealed_hash text        NOT NULL,
    state       text        NOT NULL,
    prev_hash   text        NOT NULL,
    hash        text        NOT NULL,
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    db_user     text        NOT NULL DEFAULT current_user,
    app         text        NOT NULL DEFAULT '',
    PRIMARY KEY (chain, seq)
);
CREATE INDEX IF NOT EXISTS audit_chain_link_row
    ON sage.audit_chain_link (chain, row_id, seq);
CREATE INDEX IF NOT EXISTS audit_chain_link_truncate
    ON sage.audit_chain_link (chain, seq) WHERE op = 'T';
CREATE TABLE IF NOT EXISTS sage.audit_chain_meta (
    chain         text        PRIMARY KEY,
    table_name    text        NOT NULL,
    legacy_max_id bigint      NOT NULL,
    installed_at  timestamptz NOT NULL DEFAULT now()
);
`

// ddlAppend appends one link at commit. The advisory lock orders writers;
// the head probe reads the last committed link (READ COMMITTED takes a new
// snapshot per statement), and the primary key refuses a fork outright.
var ddlAppend = fmt.Sprintf(`
CREATE OR REPLACE FUNCTION sage.audit_chain_append(
    p_chain text, p_row bigint, p_op text, p_v integer, p_sealed text, p_state text,
    OUT o_seq bigint, OUT o_prev text, OUT o_hash text)
LANGUAGE plpgsql AS $fn$
BEGIN
    PERFORM pg_advisory_xact_lock(%d, hashtext(p_chain));
    /* pg_sage audit_chain v1 */
    SELECT l.seq, l.hash INTO o_seq, o_prev FROM sage.audit_chain_link l
     WHERE l.chain = p_chain ORDER BY l.seq DESC LIMIT 1;
    IF NOT FOUND THEN
        o_seq := 0;
        o_prev := repeat('0', 64);
    END IF;
    o_seq := o_seq + 1;
    o_hash := encode(sha256(convert_to(concat_ws('|', o_prev, o_seq::text, p_op,
        p_row::text, p_v::text, p_sealed, p_state), 'UTF8')), 'hex');
    /* pg_sage audit_chain v1 */
    INSERT INTO sage.audit_chain_link (chain, seq, row_id, op, v, sealed_hash, state,
        prev_hash, hash, db_user, app)
    VALUES (p_chain, o_seq, p_row, p_op, p_v, p_sealed, p_state, o_prev, o_hash,
        current_user, coalesce(current_setting('application_name', true), ''));
END $fn$;

CREATE OR REPLACE FUNCTION sage.audit_chain_truncate() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    PERFORM sage.audit_chain_append(TG_ARGV[0], 0, 'T', 1, '', '');
    RETURN NULL;
END $fn$;
`, lockClass)

// MigrationSQL is the idempotent DDL that installs every built-in chain.
func MigrationSQL() string {
	var b strings.Builder
	b.WriteString(ddlLinkTable)
	b.WriteString(ddlAppend)
	for _, s := range Specs() {
		b.WriteString(InstallSQL(s))
	}
	return b.String()
}

// InstallSQL is the idempotent DDL that chains one table: its trigger
// function always, and the chain columns, triggers and legacy boundary
// once (and only when the table exists). It assumes the link table and
// sage.audit_chain_append exist (MigrationSQL creates them first).
func InstallSQL(s Spec) string {
	if err := s.validate(); err != nil {
		panic(err) // built-in specs are validated by tests; a bad spec is a bug
	}
	return linkFunctionSQL(s) + installBlockSQL(s)
}

// linkFunctionSQL is the chain's row trigger: it hashes the row's sealed
// columns and state, appends the link and, for an insert, writes the link
// into the row (a state-neutral update, so it appends nothing).
func linkFunctionSQL(s Spec) string {
	return fmt.Sprintf(`
CREATE OR REPLACE FUNCTION sage.audit_chain_link_%[1]s() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
    r record;
    v_state text := '';
    v_link record;
BEGIN
    IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
    IF TG_OP <> 'DELETE' THEN v_state := %[2]s; END IF;
    SELECT * INTO v_link FROM sage.audit_chain_append('%[1]s', r.id::bigint,
        left(TG_OP, 1), %[3]d, %[4]s, v_state);
    IF TG_OP = 'INSERT' THEN
        /* pg_sage audit_chain v1 */
        UPDATE %[5]s SET chain_seq = v_link.o_seq, chain_prev_hash = v_link.o_prev,
            chain_hash = v_link.o_hash WHERE id = r.id;
    END IF;
    RETURN NULL;
END $fn$;
`, s.Chain, s.stateSQL("r"), s.Version, s.sealedHashSQL("r"), s.Table)
}

// installBlockSQL adds the columns, triggers and legacy boundary once. The
// boundary is never moved: rows written while the triggers were missing
// stay unchained (a problem), not legacy.
func installBlockSQL(s Spec) string {
	triggers := ensureTrigger(s, "audit_chain_v1_id", `CREATE CONSTRAINT TRIGGER audit_chain_v1_id
            AFTER INSERT OR DELETE ON %[1]s DEFERRABLE INITIALLY DEFERRED
            FOR EACH ROW EXECUTE FUNCTION sage.audit_chain_link_%[2]s()`)
	if len(s.State) > 0 {
		triggers += ensureTrigger(s, "audit_chain_v1_u", `CREATE CONSTRAINT TRIGGER audit_chain_v1_u
            AFTER UPDATE ON %[1]s DEFERRABLE INITIALLY DEFERRED
            FOR EACH ROW WHEN (`+s.stateSQL("OLD")+` IS DISTINCT FROM `+
			s.stateSQL("NEW")+`) EXECUTE FUNCTION sage.audit_chain_link_%[2]s()`)
	}
	triggers += ensureTrigger(s, "audit_chain_v1_t", `CREATE TRIGGER audit_chain_v1_t
            BEFORE TRUNCATE ON %[1]s FOR EACH STATEMENT
            EXECUTE FUNCTION sage.audit_chain_truncate('%[2]s')`)
	return fmt.Sprintf(`
/* pg_sage audit_chain v1 */
DO $do$
BEGIN
    IF to_regclass('%[1]s') IS NULL THEN
        RETURN;
    END IF;
    PERFORM pg_advisory_xact_lock(%[3]d, hashtext('audit_chain install'));
    IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = '%[1]s'::regclass
                   AND attname = 'chain_hash' AND NOT attisdropped) THEN
        EXECUTE $q$ALTER TABLE %[1]s ADD COLUMN IF NOT EXISTS chain_seq bigint,
            ADD COLUMN IF NOT EXISTS chain_prev_hash text,
            ADD COLUMN IF NOT EXISTS chain_hash text$q$;
    END IF;%[4]s
    INSERT INTO sage.audit_chain_meta (chain, table_name, legacy_max_id)
    SELECT '%[2]s', '%[1]s', coalesce(max(id), 0) FROM %[1]s
    ON CONFLICT (chain) DO NOTHING;
END $do$;
`, s.Table, s.Chain, lockClass, triggers)
}

// ensureTrigger creates one chain trigger when it is missing. The table's
// SHARE ROW EXCLUSIVE lock from CREATE TRIGGER waits out in-flight writers,
// so no committed row escapes both the boundary and the trigger.
func ensureTrigger(s Spec, name, create string) string {
	return fmt.Sprintf(`
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = '%[1]s'::regclass
                   AND tgname = '`+name+`') THEN
        EXECUTE $q$`+create+`$q$;
    END IF;`, s.Table, s.Chain)
}
