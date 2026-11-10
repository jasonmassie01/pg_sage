package schema

// ddlFactEvents adds the history of fact decisions (facts-07).
//
// sage.fact_events is append-only: one row per status or decision change
// of a fact, written by triggers on sage.facts, so every writer (REST,
// chat cards, MCP, the verifier, column classes) is covered inside its own
// transaction and a refused event fails the change. It has no foreign key:
// the history outlives a fact retention purges. fact_hash is the fact's
// Hash() after the change (id, type, kind, subject, status and value), so
// an event names exactly the fact a card or decision was bound to. Only
// the audit chain's own columns may be written after the insert; the
// chain itself (auditchain.FactEvents) is installed by the audit chain
// migration, Bootstrap's last step. The first install records the state
// of every older fact once (verb baseline; see ddlFactEventsBaseline).
//
// sage.facts.expired_by names who expired a fact (a person, or pg_sage's
// verifier). A nullable column without a default: no table rewrite on
// PG 14-18. Catalog checks first keep a re-run from taking any lock on
// sage.facts; lock_timeout bounds the first run.
const ddlFactEvents = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.fact_events (
    id          bigserial   PRIMARY KEY,
    fact_id     bigint      NOT NULL,
    at          timestamptz NOT NULL DEFAULT now(),
    actor       text        NOT NULL CHECK (length(actor) BETWEEN 1 AND 200),
    verb        text        NOT NULL CHECK (verb IN ('proposed', 'declared', 'confirmed',
                    'rejected', 'expired', 'reopened', 'restored', 'baseline')),
    from_status text        NOT NULL DEFAULT '',
    to_status   text        NOT NULL,
    fact_hash   text        NOT NULL CHECK (length(fact_hash) = 32),
    note        text        NOT NULL DEFAULT '' CHECK (length(note) <= 1000)
);
CREATE INDEX IF NOT EXISTS fact_events_fact ON sage.fact_events (fact_id, id);
CREATE INDEX IF NOT EXISTS fact_events_at ON sage.fact_events (at);
` + ddlFactHash + ddlFactEventsGuard + ddlFactEventsRecord + `
DO $fact_events$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'sage.facts'::regclass
                   AND attname = 'expired_by' AND NOT attisdropped) THEN
        ALTER TABLE sage.facts ADD COLUMN expired_by text
            CHECK (length(expired_by) <= 200);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'sage.fact_events'::regclass
                   AND tgname = 'fact_events_append_only') THEN
        CREATE TRIGGER fact_events_append_only BEFORE UPDATE ON sage.fact_events
            FOR EACH ROW EXECUTE FUNCTION sage.fact_events_append_only();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'sage.facts'::regclass
                   AND tgname = 'fact_events_insert') THEN
        -- Writers wait while the older facts' state is recorded, so none
        -- slips between the baseline and the triggers.
        LOCK TABLE sage.facts IN SHARE ROW EXCLUSIVE MODE;
        IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'sage.facts'::regclass
                       AND tgname = 'fact_events_insert') THEN
            ` + ddlFactEventsBaseline + `
            CREATE TRIGGER fact_events_insert AFTER INSERT ON sage.facts
                FOR EACH ROW EXECUTE FUNCTION sage.fact_events_record();
        END IF;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'sage.facts'::regclass
                   AND tgname = 'fact_events_update') THEN
        CREATE TRIGGER fact_events_update AFTER UPDATE ON sage.facts
            FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status
                OR OLD.decided_by IS DISTINCT FROM NEW.decided_by
                OR OLD.decided_at IS DISTINCT FROM NEW.decided_at)
            EXECUTE FUNCTION sage.fact_events_record();
    END IF;
END
$fact_events$;
`

// ddlFactEventsGuard refuses any update of an event except the audit
// chain writing its own columns into the row it just linked.
const ddlFactEventsGuard = `
CREATE OR REPLACE FUNCTION sage.fact_events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
    IF (NEW.id, NEW.fact_id, NEW.at, NEW.actor, NEW.verb, NEW.from_status,
        NEW.to_status, NEW.fact_hash, NEW.note) IS DISTINCT FROM
       (OLD.id, OLD.fact_id, OLD.at, OLD.actor, OLD.verb, OLD.from_status,
        OLD.to_status, OLD.fact_hash, OLD.note) THEN
        RAISE EXCEPTION 'pg_sage: sage.fact_events is append-only (event %)', OLD.id
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END $fn$;
`

// ddlFactEventsRecord writes one event for an inserted fact or a status or
// decision change. The verb and actor follow from the change: a
// declaration is the one decision that also counts a proposal; a restored
// confirmation (facts-08) keeps its decider and is pg_sage's doing; an
// expiry names expired_by (pg_sage when a writer set none).
const ddlFactEventsRecord = `
CREATE OR REPLACE FUNCTION sage.fact_events_record() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
    v_from     text := '';
    v_declared boolean := TG_OP = 'INSERT';
    v_restored boolean := false;
    v_verb     text := NEW.status;
    v_actor    text := NEW.decided_by;
    v_note     text := NEW.decision_note;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        v_from := OLD.status;
        v_declared := NEW.proposals <> OLD.proposals;
        v_restored := OLD.status = 'expired'
            AND NEW.decided_at IS NOT DISTINCT FROM OLD.decided_at;
    END IF;
    IF NEW.status = 'expired' THEN
        v_actor := COALESCE(NULLIF(NEW.expired_by, ''), 'pg_sage');
        v_note := NEW.expired_reason;
    ELSIF NEW.status = 'proposed' THEN
        v_verb := CASE WHEN v_from = 'expired' THEN 'reopened' ELSE 'proposed' END;
        v_actor := COALESCE(NULLIF(NEW.proposed_by, ''), NEW.source);
        v_note := '';
    ELSIF NEW.status = 'confirmed' AND v_restored THEN
        v_verb := 'restored';
        v_actor := 'pg_sage';
    ELSIF NEW.status = 'confirmed' AND v_declared THEN
        v_verb := 'declared';
    END IF;
    INSERT INTO sage.fact_events (fact_id, actor, verb, from_status, to_status,
        fact_hash, note)
    VALUES (NEW.id, left(COALESCE(NULLIF(v_actor, ''), 'unknown'), 200), v_verb, v_from,
        NEW.status, sage.fact_hash(NEW), left(COALESCE(v_note, ''), 1000));
    RETURN NULL;
END $fn$;
`

// ddlFactHash is Fact.Hash() in SQL: sha256 over id, type, kind, subject,
// status and the value's keys in byte order (COLLATE "C", like
// sort.Strings), the first 16 bytes in hex. A value that is not a JSON
// object hashes as {}.
const ddlFactHash = `
CREATE OR REPLACE FUNCTION sage.fact_hash(f sage.facts) RETURNS text
LANGUAGE sql STABLE AS $fn$
    SELECT left(encode(sha256(convert_to(concat_ws(chr(31), f.id::text, f.fact_type,
            f.subject_kind, f.subject, f.status)
        || COALESCE((SELECT string_agg(chr(31) || e.key || '=' || e.value, ''
                                       ORDER BY e.key COLLATE "C")
                     FROM jsonb_each_text(CASE WHEN jsonb_typeof(f.value) = 'object'
                          THEN f.value ELSE '{}'::jsonb END) e), ''),
        'UTF8')), 'hex'), 32)
$fn$;
`

// ddlFactEventsBaseline records, once, the state of each fact that has no
// event yet (decided before facts-07), so the first revocation after the
// upgrade still shows who decided it. The row keeps only its latest
// decision: the event names that decider and time; who expired a fact
// was never recorded, so an expiry's actor is unknown and its note keeps
// the decider and the reason.
const ddlFactEventsBaseline = `INSERT INTO sage.fact_events (fact_id, at, actor, verb,
                from_status, to_status, fact_hash, note)
            SELECT f.id, COALESCE(f.decided_at, f.created_at),
                left(COALESCE(NULLIF(CASE
                    WHEN f.status IN ('confirmed', 'rejected') THEN f.decided_by
                    WHEN f.status = 'proposed' THEN COALESCE(NULLIF(f.proposed_by, ''),
                                                             f.source)
                    END, ''), 'unknown'), 200),
                'baseline', '', f.status, sage.fact_hash(f),
                left('state when the fact history began (facts-07)' || COALESCE(': ' ||
                    NULLIF(concat_ws('; ',
                        CASE WHEN f.status = 'expired' AND f.decided_by <> ''
                             THEN 'decided by ' || f.decided_by END,
                        CASE WHEN f.status = 'expired' THEN NULLIF(f.expired_reason, '')
                             WHEN f.status IN ('confirmed', 'rejected')
                             THEN NULLIF(f.decision_note, '') END), ''), ''), 1000)
            FROM sage.facts f
            WHERE NOT EXISTS (SELECT 1 FROM sage.fact_events e WHERE e.fact_id = f.id)
            ORDER BY f.id;`
