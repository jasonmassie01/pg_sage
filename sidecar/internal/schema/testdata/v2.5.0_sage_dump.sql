--
-- PostgreSQL database dump
--


-- Dumped from database version 14.23 (Debian 14.23-1.pgdg12+1)
-- Dumped by pg_dump version 14.23 (Debian 14.23-1.pgdg12+1)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: sage; Type: SCHEMA; Schema: -; Owner: -
--

CREATE SCHEMA sage;


--
-- Name: audit_chain_append(text, bigint, text, integer, text, text); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.audit_chain_append(p_chain text, p_row bigint, p_op text, p_v integer, p_sealed text, p_state text, OUT o_seq bigint, OUT o_prev text, OUT o_hash text) RETURNS record
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(1885827937, hashtext(p_chain));
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
END $$;


--
-- Name: audit_chain_link_action_log(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.audit_chain_link_action_log() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
    v_state text := '';
    v_link record;
BEGIN
    IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
    IF TG_OP <> 'DELETE' THEN v_state := jsonb_build_array(r.outcome, r.rollback_reason, (extract(epoch FROM r.measured_at) * 1000000)::bigint, r.after_state, r.verification_id, r.justification, r.toil_minutes_saved, r.toil_model_version)::text; END IF;
    SELECT * INTO v_link FROM sage.audit_chain_append('action_log', r.id::bigint,
        left(TG_OP, 1), 1, encode(sha256(convert_to(jsonb_build_array(r.id, (extract(epoch FROM r.executed_at) * 1000000)::bigint, r.action_type, r.finding_id, r.sql_executed, r.rollback_sql, r.before_state, r.approved_by, (extract(epoch FROM r.approved_at) * 1000000)::bigint, r.decision_id, r.database_id)::text, 'UTF8')), 'hex'), v_state);
    IF TG_OP = 'INSERT' THEN
        /* pg_sage audit_chain v1 */
        UPDATE sage.action_log SET chain_seq = v_link.o_seq, chain_prev_hash = v_link.o_prev,
            chain_hash = v_link.o_hash WHERE id = r.id;
    END IF;
    RETURN NULL;
END $$;


--
-- Name: audit_chain_link_auth_audit(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.audit_chain_link_auth_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
    v_state text := '';
    v_link record;
BEGIN
    IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
    IF TG_OP <> 'DELETE' THEN v_state := ''; END IF;
    SELECT * INTO v_link FROM sage.audit_chain_append('auth_audit', r.id::bigint,
        left(TG_OP, 1), 1, encode(sha256(convert_to(jsonb_build_array(r.id, r.event, r.actor_user_id, r.target_user_id, r.detail, (extract(epoch FROM r.created_at) * 1000000)::bigint, r.source_ip)::text, 'UTF8')), 'hex'), v_state);
    IF TG_OP = 'INSERT' THEN
        /* pg_sage audit_chain v1 */
        UPDATE sage.auth_audit SET chain_seq = v_link.o_seq, chain_prev_hash = v_link.o_prev,
            chain_hash = v_link.o_hash WHERE id = r.id;
    END IF;
    RETURN NULL;
END $$;


--
-- Name: audit_chain_link_config_audit(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.audit_chain_link_config_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
    v_state text := '';
    v_link record;
BEGIN
    IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
    IF TG_OP <> 'DELETE' THEN v_state := ''; END IF;
    SELECT * INTO v_link FROM sage.audit_chain_append('config_audit', r.id::bigint,
        left(TG_OP, 1), 1, encode(sha256(convert_to(jsonb_build_array(r.id, r.key, r.old_value, r.new_value, r.database_id, r.changed_by, (extract(epoch FROM r.changed_at) * 1000000)::bigint, r.changed_by_actor)::text, 'UTF8')), 'hex'), v_state);
    IF TG_OP = 'INSERT' THEN
        /* pg_sage audit_chain v1 */
        UPDATE sage.config_audit SET chain_seq = v_link.o_seq, chain_prev_hash = v_link.o_prev,
            chain_hash = v_link.o_hash WHERE id = r.id;
    END IF;
    RETURN NULL;
END $$;


--
-- Name: audit_chain_link_guard_query_audit(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.audit_chain_link_guard_query_audit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
    v_state text := '';
    v_link record;
BEGIN
    IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
    IF TG_OP <> 'DELETE' THEN v_state := ''; END IF;
    SELECT * INTO v_link FROM sage.audit_chain_append('guard_query_audit', r.id::bigint,
        left(TG_OP, 1), 1, encode(sha256(convert_to(jsonb_build_array(r.id, r.database_id::text, r.principal_id, r.task_id, r.envelope_hash, r.verdict, r.reason, r.row_count, r.classes, (extract(epoch FROM r.at) * 1000000)::bigint)::text, 'UTF8')), 'hex'), v_state);
    IF TG_OP = 'INSERT' THEN
        /* pg_sage audit_chain v1 */
        UPDATE sage.guard_query_audit SET chain_seq = v_link.o_seq, chain_prev_hash = v_link.o_prev,
            chain_hash = v_link.o_hash WHERE id = r.id;
    END IF;
    RETURN NULL;
END $$;


--
-- Name: audit_chain_link_pgaudit_events(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.audit_chain_link_pgaudit_events() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
    v_state text := '';
    v_link record;
BEGIN
    IF TG_OP = 'DELETE' THEN r := OLD; ELSE r := NEW; END IF;
    IF TG_OP <> 'DELETE' THEN v_state := ''; END IF;
    SELECT * INTO v_link FROM sage.audit_chain_append('pgaudit_events', r.id::bigint,
        left(TG_OP, 1), 1, encode(sha256(convert_to(jsonb_build_array(r.id, r.database_name, (extract(epoch FROM r.logged_at) * 1000000)::bigint, r.session_id, r.pid, r.db_user, r.application, r.principal_id, r.action_id, r.audit_type, r.statement_id, r.substatement_id, r.class, r.command, r.object_type, r.object_name, r.statement, r.correlated_by)::text, 'UTF8')), 'hex'), v_state);
    IF TG_OP = 'INSERT' THEN
        /* pg_sage audit_chain v1 */
        UPDATE sage.guard_pgaudit_events SET chain_seq = v_link.o_seq, chain_prev_hash = v_link.o_prev,
            chain_hash = v_link.o_hash WHERE id = r.id;
    END IF;
    RETURN NULL;
END $$;


--
-- Name: audit_chain_truncate(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.audit_chain_truncate() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM sage.audit_chain_append(TG_ARGV[0], 0, 'T', 1, '', '');
    RETURN NULL;
END $$;


--
-- Name: recommendation_immutable(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.recommendation_immutable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'sage.% rows are immutable', TG_TABLE_NAME;
END $$;


--
-- Name: snapshot_apply(jsonb, jsonb); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.snapshot_apply(p_base jsonb, p_delta jsonb) RETURNS jsonb
    LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE
    AS $_$
DECLARE
    v_global  jsonb := COALESCE(p_delta->'g', '{}');
    v_inc     jsonb := COALESCE(p_delta->'i', '{}');
    v_upd     jsonb := COALESCE(p_delta->'u', '{}');
    v_added   jsonb := COALESCE(p_delta->'a', '[]');
    v_removed jsonb := COALESCE(p_delta->'d', '[]');
    v_order   jsonb := p_delta->'o';
    v_nbase   int;
    v_result  jsonb;
BEGIN
    IF p_delta ? 'w' THEN -- an object document, encoded as a list of one
        IF jsonb_typeof(p_base) <> 'object' THEN
            RETURN NULL;
        END IF;
        RETURN sage.snapshot_apply(jsonb_build_array(p_base), p_delta - 'w')->0;
    END IF;
    IF jsonb_typeof(p_base) <> 'array' THEN
        RETURN NULL; -- a delta is only ever written against a list
    END IF;
    v_nbase := jsonb_array_length(p_base);
    IF v_nbase = (p_delta->>'n')::int AND NOT (p_delta ?| ARRAY['g', 'i', 'u', 'a', 'd', 'o'])
    THEN
        RETURN p_base; -- nothing changed
    END IF;
    WITH base AS (
        -- g: an increment for every element whose field is an integer.
        SELECT b.ord - 1 AS idx,
               CASE WHEN v_global = '{}' THEN b.elem
               ELSE b.elem || COALESCE((
                   SELECT jsonb_object_agg(g.key, to_jsonb(
                              (b.elem->>g.key)::numeric + (g.value#>>'{}')::numeric))
                     FROM jsonb_each(v_global) g
                    WHERE jsonb_typeof(b.elem->g.key) = 'number'
                      AND b.elem->>g.key ~ '^-?(0|[1-9][0-9]*)$'), '{}')
               END AS elem
          FROM jsonb_array_elements(p_base) WITH ORDINALITY b(elem, ord)
    ), incs AS (
        -- i: per element increments, after g.
        SELECT base.idx,
               jsonb_object_agg(f.key, to_jsonb(
                   (base.elem->>f.key)::numeric + (f.value#>>'{}')::numeric)) AS patch
          FROM jsonb_each(v_inc) e
          JOIN base ON base.idx = e.key::int
          CROSS JOIN LATERAL jsonb_each(e.value) f
         GROUP BY base.idx
    ), items AS (
        -- u: per element values, last.
        SELECT base.idx, false AS added,
               base.elem || COALESCE(incs.patch, '{}') || COALESCE(u.value, '{}') AS item
          FROM base
          LEFT JOIN incs ON incs.idx = base.idx
          LEFT JOIN jsonb_each(v_upd) u ON u.key::int = base.idx
        UNION ALL
        SELECT v_nbase + a.ord - 1, true, a.elem
          FROM jsonb_array_elements(v_added) WITH ORDINALITY a(elem, ord)
    )
    SELECT CASE WHEN v_order IS NULL THEN
               (SELECT COALESCE(jsonb_agg(i.item ORDER BY i.idx), '[]')
                  FROM items i
                 WHERE i.added
                    OR NOT EXISTS (SELECT 1 FROM jsonb_array_elements_text(v_removed) r
                                    WHERE r.value::int = i.idx))
           ELSE
               (SELECT COALESCE(jsonb_agg(i.item ORDER BY o.pos), '[]')
                  FROM jsonb_array_elements_text(v_order) WITH ORDINALITY o(idx, pos)
                  JOIN items i ON i.idx = o.idx::int)
           END
      INTO v_result;
    RETURN v_result;
END
$_$;


--
-- Name: snapshot_data(jsonb, bigint); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.snapshot_data(p_data jsonb, p_base_id bigint) RETURNS jsonb
    LANGUAGE plpgsql STABLE PARALLEL SAFE
    AS $$
DECLARE
    v_data    jsonb := p_data;
    v_base_id bigint := p_base_id;
    v_chain   jsonb[] := '{}';
    v_depth   int := 0;
BEGIN
    -- Walk to the full row, then apply the deltas oldest first. The writer
    -- keeps chains at most two deep; a longer one (manual edits) is refused
    -- as unreadable rather than followed forever.
    WHILE v_base_id IS NOT NULL LOOP
        v_depth := v_depth + 1;
        IF v_depth > 8 THEN
            RETURN NULL;
        END IF;
        v_chain := array_prepend(v_data, v_chain);
        SELECT b.data, b.base_id INTO v_data, v_base_id
          FROM sage.snapshots b WHERE b.id = v_base_id;
        IF NOT FOUND THEN
            RETURN NULL; -- the base aged out or was deleted
        END IF;
    END LOOP;
    FOR i IN 1 .. COALESCE(array_length(v_chain, 1), 0) LOOP
        v_data := sage.snapshot_apply(v_data, v_chain[i]);
    END LOOP;
    RETURN v_data;
END
$$;


--
-- Name: snapshot_data(jsonb, bigint, timestamp with time zone); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.snapshot_data(p_data jsonb, p_base_id bigint, p_at timestamp with time zone) RETURNS jsonb
    LANGUAGE plpgsql STABLE PARALLEL SAFE
    AS $$
DECLARE
    v_data    jsonb := p_data;
    v_base_id bigint := p_base_id;
    v_at      timestamptz := p_at;
    v_chain   jsonb[] := '{}';
    v_depth   int := 0;
    r         record;
BEGIN
    WHILE v_base_id IS NOT NULL LOOP
        v_depth := v_depth + 1;
        IF v_depth > 8 THEN
            RETURN NULL;
        END IF;
        v_chain := array_prepend(v_data, v_chain);
        SELECT b.data, b.base_id, b.collected_at INTO r
          FROM sage.snapshots b
         WHERE b.id = v_base_id AND v_at IS NOT NULL
           AND b.collected_at >= date_trunc('day', v_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
           AND b.collected_at <= v_at;
        IF NOT FOUND THEN
            SELECT b.data, b.base_id, b.collected_at INTO r
              FROM sage.snapshots b WHERE b.id = v_base_id;
            IF NOT FOUND THEN
                RETURN NULL; -- the base aged out or was deleted
            END IF;
        END IF;
        v_data := r.data;
        v_base_id := r.base_id;
        v_at := r.collected_at;
    END LOOP;
    FOR i IN 1 .. COALESCE(array_length(v_chain, 1), 0) LOOP
        v_data := sage.snapshot_apply(v_data, v_chain[i]);
    END LOOP;
    RETURN v_data;
END
$$;


--
-- Name: sre_append_only(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.sre_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'sage.% is append-only', TG_TABLE_NAME
        USING ERRCODE = 'insufficient_privilege';
END $$;


--
-- Name: sre_runbook_version_guard(); Type: FUNCTION; Schema: sage; Owner: -
--

CREATE FUNCTION sage.sre_runbook_version_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'sage.sre_runbook_versions is append-only: a runbook version '
            'cannot be deleted' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.signed_at IS NOT NULL THEN
        RAISE EXCEPTION 'a signed runbook version cannot change'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF (NEW.deployment_id, NEW.database_id, NEW.runbook_id, NEW.version, NEW.name,
        NEW.definition, NEW.content_hash, NEW.source, NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       (OLD.deployment_id, OLD.database_id, OLD.runbook_id, OLD.version, OLD.name,
        OLD.definition, OLD.content_hash, OLD.source, OLD.created_by, OLD.created_at)
       OR NEW.source_text IS DISTINCT FROM OLD.source_text
       OR NEW.compiled_by IS DISTINCT FROM OLD.compiled_by THEN
        RAISE EXCEPTION 'a runbook version''s content is immutable'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: action_log; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.action_log (
    id bigint NOT NULL,
    executed_at timestamp with time zone DEFAULT now() NOT NULL,
    action_type text NOT NULL,
    finding_id bigint,
    sql_executed text NOT NULL,
    rollback_sql text,
    before_state jsonb,
    after_state jsonb,
    outcome text DEFAULT 'pending'::text NOT NULL,
    rollback_reason text,
    measured_at timestamp with time zone,
    approved_by integer,
    approved_at timestamp with time zone,
    justification text,
    decision_id bigint,
    verification_id bigint,
    database_id bigint,
    toil_minutes_saved numeric,
    toil_model_version integer,
    principal_id text,
    on_behalf_of text,
    task_id text,
    approval_id bigint,
    envelope_id text,
    artifact_hash text,
    policy_version integer,
    chain_seq bigint,
    chain_prev_hash text,
    chain_hash text
);


--
-- Name: action_log_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.action_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: action_log_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.action_log_id_seq OWNED BY sage.action_log.id;


--
-- Name: action_outcome; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.action_outcome (
    action_log_id bigint NOT NULL,
    database_id bigint,
    action_class text NOT NULL,
    predicted jsonb NOT NULL,
    prediction_method text NOT NULL,
    verdict text DEFAULT 'pending'::text NOT NULL,
    tolerance text DEFAULT 'pending'::text NOT NULL,
    observed jsonb DEFAULT '{}'::jsonb NOT NULL,
    evidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    window_start timestamp with time zone,
    window_end timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    decided_at timestamp with time zone,
    CONSTRAINT action_outcome_method CHECK ((prediction_method = ANY (ARRAY['hypopg'::text, 'model'::text, 'rule'::text, 'none'::text]))),
    CONSTRAINT action_outcome_tolerance CHECK ((tolerance = ANY (ARRAY['pending'::text, 'met'::text, 'partial'::text, 'missed'::text, 'no_prediction'::text, 'unmeasured'::text]))),
    CONSTRAINT action_outcome_verdict CHECK ((verdict = ANY (ARRAY['pending'::text, 'improved'::text, 'neutral'::text, 'regressed'::text, 'insufficient_evidence'::text, 'unverifiable'::text])))
);


--
-- Name: action_queue; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.action_queue (
    id integer NOT NULL,
    database_id integer,
    finding_id integer,
    proposed_sql text NOT NULL,
    rollback_sql text,
    action_risk text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    proposed_at timestamp with time zone DEFAULT now(),
    decided_by integer,
    decided_at timestamp with time zone,
    expires_at timestamp with time zone DEFAULT (now() + '7 days'::interval),
    reason text,
    action_type text,
    identity_key text,
    policy_decision text,
    guardrails jsonb DEFAULT '[]'::jsonb NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    last_attempt_at timestamp with time zone,
    cooldown_until timestamp with time zone,
    failure_fingerprint text,
    last_failure_fingerprint text,
    verification_status text DEFAULT 'not_started'::text NOT NULL,
    shadow_toil_minutes integer DEFAULT 0 NOT NULL,
    action_log_id bigint,
    recommendation_id bigint,
    recommendation_revision integer,
    content_hash text,
    snoozed_until timestamp with time zone,
    snoozed_by integer,
    snooze_reason text,
    proposed_via text,
    proposed_by text,
    principal_id text,
    artifact_hash text,
    CONSTRAINT action_queue_proposed_via_v2 CHECK ((((proposed_via IS NULL) OR (proposed_via = ANY (ARRAY['ask_sage'::text, 'agent'::text]))) AND ((proposed_by IS NULL) OR ((length(proposed_by) >= 1) AND (length(proposed_by) <= 200)))))
)
WITH (autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: action_queue_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.action_queue_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: action_queue_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.action_queue_id_seq OWNED BY sage.action_queue.id;


--
-- Name: admission_withheld; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.admission_withheld (
    id bigint NOT NULL,
    database_name text NOT NULL,
    finding_key text NOT NULL,
    reason text NOT NULL,
    mode text NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    evidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    decision_id bigint,
    first_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    occurrences integer DEFAULT 1 NOT NULL,
    CONSTRAINT admission_withheld_occurrences_check CHECK ((occurrences > 0))
);


--
-- Name: admission_withheld_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.admission_withheld_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: admission_withheld_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.admission_withheld_id_seq OWNED BY sage.admission_withheld.id;


--
-- Name: agentdb_decommission; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.agentdb_decommission (
    resource_id text NOT NULL,
    exported boolean NOT NULL,
    acknowledged_by text NOT NULL,
    source text NOT NULL,
    acknowledged_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT agentdb_decommission_acknowledged_by_check CHECK ((acknowledged_by <> ''::text)),
    CONSTRAINT agentdb_decommission_exported_check CHECK (exported),
    CONSTRAINT agentdb_decommission_source_check CHECK ((source = ANY (ARRAY['api'::text, 'yaml'::text])))
);


--
-- Name: alert_log; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.alert_log (
    id bigint NOT NULL,
    sent_at timestamp with time zone DEFAULT now() NOT NULL,
    finding_id bigint,
    severity text NOT NULL,
    channel text NOT NULL,
    dedup_key text NOT NULL,
    status text DEFAULT 'sent'::text NOT NULL,
    error_message text
);


--
-- Name: alert_log_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.alert_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: alert_log_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.alert_log_id_seq OWNED BY sage.alert_log.id;


--
-- Name: approval_card_deliveries; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.approval_card_deliveries (
    id bigint NOT NULL,
    token_sha256 bytea NOT NULL,
    channel_id integer NOT NULL,
    database_name text NOT NULL,
    queue_id integer NOT NULL,
    card_hash text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    summary text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone,
    used_by integer,
    decision text,
    message_id bigint DEFAULT 0 NOT NULL,
    followed_up_at timestamp with time zone,
    followup_verdict text DEFAULT ''::text NOT NULL,
    CONSTRAINT approval_card_deliveries_card_hash_check CHECK (((length(card_hash) >= 1) AND (length(card_hash) <= 128))),
    CONSTRAINT approval_card_deliveries_channel_id_check CHECK ((channel_id > 0)),
    CONSTRAINT approval_card_deliveries_check CHECK (((used_at IS NULL) = (decision IS NULL))),
    CONSTRAINT approval_card_deliveries_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 128))),
    CONSTRAINT approval_card_deliveries_decision_check CHECK ((decision = ANY (ARRAY['approve'::text, 'deny'::text, 'snooze'::text]))),
    CONSTRAINT approval_card_deliveries_followup_verdict_check CHECK ((length(followup_verdict) <= 64)),
    CONSTRAINT approval_card_deliveries_queue_id_check CHECK ((queue_id > 0)),
    CONSTRAINT approval_card_deliveries_summary_check CHECK ((length(summary) <= 512)),
    CONSTRAINT approval_card_deliveries_title_check CHECK ((length(title) <= 512)),
    CONSTRAINT approval_card_deliveries_token_sha256_check CHECK ((octet_length(token_sha256) = 32))
);


--
-- Name: approval_card_deliveries_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.approval_card_deliveries_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: approval_card_deliveries_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.approval_card_deliveries_id_seq OWNED BY sage.approval_card_deliveries.id;


--
-- Name: ask_budget_day; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.ask_budget_day (
    day date NOT NULL,
    actor text NOT NULL,
    tokens bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ask_budget_day_actor_check CHECK (((length(actor) >= 1) AND (length(actor) <= 200))),
    CONSTRAINT ask_budget_day_tokens_check CHECK ((tokens >= 0))
);


--
-- Name: ask_conversations; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.ask_conversations (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    actor text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ask_conversations_actor_check CHECK (((length(actor) >= 1) AND (length(actor) <= 200))),
    CONSTRAINT ask_conversations_title_check CHECK ((length(title) <= 200))
);


--
-- Name: ask_messages; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.ask_messages (
    id bigint NOT NULL,
    conversation_id uuid NOT NULL,
    question text NOT NULL,
    answer jsonb NOT NULL,
    status text NOT NULL,
    tokens integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ask_messages_question_check CHECK (((length(question) >= 1) AND (length(question) <= 8000))),
    CONSTRAINT ask_messages_status_check CHECK ((status = ANY (ARRAY['answered'::text, 'not_observed'::text, 'budget_exhausted'::text, 'llm_unavailable'::text, 'incomplete'::text]))),
    CONSTRAINT ask_messages_tokens_check CHECK ((tokens >= 0))
);


--
-- Name: ask_messages_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.ask_messages_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: ask_messages_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.ask_messages_id_seq OWNED BY sage.ask_messages.id;


--
-- Name: audit_chain_link; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.audit_chain_link (
    chain text NOT NULL,
    seq bigint NOT NULL,
    row_id bigint NOT NULL,
    op text NOT NULL,
    v integer NOT NULL,
    sealed_hash text NOT NULL,
    state text NOT NULL,
    prev_hash text NOT NULL,
    hash text NOT NULL,
    at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    db_user text DEFAULT CURRENT_USER NOT NULL,
    app text DEFAULT ''::text NOT NULL,
    CONSTRAINT audit_chain_link_op_check CHECK ((op = ANY (ARRAY['I'::text, 'U'::text, 'D'::text, 'T'::text])))
);


--
-- Name: audit_chain_meta; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.audit_chain_meta (
    chain text NOT NULL,
    table_name text NOT NULL,
    legacy_max_id bigint NOT NULL,
    installed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: auth_audit; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.auth_audit (
    id bigint NOT NULL,
    event text NOT NULL,
    actor_user_id integer DEFAULT 0 NOT NULL,
    target_user_id integer DEFAULT 0 NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    source_ip text DEFAULT ''::text NOT NULL,
    chain_seq bigint,
    chain_prev_hash text,
    chain_hash text
);


--
-- Name: auth_audit_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.auth_audit_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: auth_audit_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.auth_audit_id_seq OWNED BY sage.auth_audit.id;


--
-- Name: briefings; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.briefings (
    id bigint NOT NULL,
    generated_at timestamp with time zone DEFAULT now() NOT NULL,
    period_start timestamp with time zone NOT NULL,
    period_end timestamp with time zone NOT NULL,
    mode text NOT NULL,
    content_text text NOT NULL,
    content_json jsonb NOT NULL,
    llm_used boolean DEFAULT false NOT NULL,
    token_count integer,
    delivery_status jsonb
);


--
-- Name: briefings_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.briefings_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: briefings_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.briefings_id_seq OWNED BY sage.briefings.id;


--
-- Name: change_lease; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.change_lease (
    id bigint NOT NULL,
    database_id bigint,
    object_key text NOT NULL,
    decision_id bigint NOT NULL,
    holder text NOT NULL,
    intent text NOT NULL,
    acquired_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    released_at timestamp with time zone,
    state text DEFAULT 'active'::text NOT NULL,
    actor text,
    object_type text,
    object_oid oid,
    object_name text,
    CONSTRAINT change_lease_state_check CHECK ((state = ANY (ARRAY['active'::text, 'released'::text, 'expired'::text])))
)
WITH (autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: change_lease_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.change_lease_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: change_lease_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.change_lease_id_seq OWNED BY sage.change_lease.id;


--
-- Name: chatops_identities; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.chatops_identities (
    id integer NOT NULL,
    provider text NOT NULL,
    team_id text DEFAULT ''::text NOT NULL,
    external_user_id text NOT NULL,
    user_id integer NOT NULL,
    created_by integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chatops_identities_external_user_id_check CHECK (((length(external_user_id) >= 1) AND (length(external_user_id) <= 128))),
    CONSTRAINT chatops_identities_provider_check CHECK ((provider = ANY (ARRAY['slack'::text, 'telegram'::text]))),
    CONSTRAINT chatops_identities_team_id_check CHECK ((length(team_id) <= 64))
);


--
-- Name: chatops_identities_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.chatops_identities_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: chatops_identities_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.chatops_identities_id_seq OWNED BY sage.chatops_identities.id;


--
-- Name: chatops_replay; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.chatops_replay (
    provider text NOT NULL,
    nonce text NOT NULL,
    seen_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT chatops_replay_nonce_check CHECK (((length(nonce) >= 1) AND (length(nonce) <= 256))),
    CONSTRAINT chatops_replay_provider_check CHECK (((length(provider) >= 1) AND (length(provider) <= 32)))
);


--
-- Name: clone_instances; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.clone_instances (
    id bigint NOT NULL,
    deployment_id uuid NOT NULL,
    adapter text NOT NULL,
    scope text NOT NULL,
    ref text,
    name text NOT NULL,
    purpose text NOT NULL,
    principal_id text,
    source_database_id uuid,
    masked boolean DEFAULT false NOT NULL,
    status text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT clone_instances_purpose_check CHECK ((purpose = ANY (ARRAY['rehearsal'::text, 'sandbox'::text, 'drill'::text, 'gameday'::text, 'bench'::text, 'masked_parent'::text]))),
    CONSTRAINT clone_instances_status_check CHECK ((status = ANY (ARRAY['creating'::text, 'create_uncertain'::text, 'ready'::text, 'destroying'::text, 'destroyed'::text, 'failed'::text])))
);


--
-- Name: clone_instances_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.clone_instances_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: clone_instances_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.clone_instances_id_seq OWNED BY sage.clone_instances.id;


--
-- Name: config; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.config (
    key text NOT NULL,
    value text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text,
    database_id integer,
    updated_by_user_id integer
);


--
-- Name: config_audit; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.config_audit (
    id integer NOT NULL,
    key text NOT NULL,
    old_value text,
    new_value text NOT NULL,
    database_id integer,
    changed_by integer,
    changed_at timestamp with time zone DEFAULT now(),
    changed_by_actor text,
    chain_seq bigint,
    chain_prev_hash text,
    chain_hash text
);


--
-- Name: config_audit_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.config_audit_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: config_audit_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.config_audit_id_seq OWNED BY sage.config_audit.id;


--
-- Name: config_derivation; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.config_derivation (
    id bigint NOT NULL,
    key text NOT NULL,
    event text NOT NULL,
    value double precision,
    previous_value double precision,
    evidence jsonb DEFAULT '[]'::jsonb NOT NULL,
    bounds jsonb DEFAULT '{}'::jsonb NOT NULL,
    rule text NOT NULL,
    rule_version integer NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    actor text DEFAULT 'pg_sage'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT config_derivation_event_check CHECK ((event = ANY (ARRAY['shadow'::text, 'promoted'::text, 'applied'::text, 'held'::text, 'cleared'::text, 'operator_set'::text, 'resumed'::text, 'pinned'::text, 'unpinned'::text]))),
    CONSTRAINT config_derivation_rule_version_check CHECK ((rule_version >= 1))
);


--
-- Name: config_derivation_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.config_derivation_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: config_derivation_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.config_derivation_id_seq OWNED BY sage.config_derivation.id;


--
-- Name: config_derived_setting; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.config_derived_setting (
    key text NOT NULL,
    status text NOT NULL,
    value double precision NOT NULL,
    pending_value double precision,
    active_value double precision,
    shadow_value double precision,
    shadow_since timestamp with time zone,
    shadow_reason text DEFAULT ''::text NOT NULL,
    shadow_outcome text DEFAULT ''::text NOT NULL,
    samples jsonb DEFAULT '[]'::jsonb NOT NULL,
    pinned_value double precision,
    pinned_by text DEFAULT ''::text NOT NULL,
    pinned_at timestamp with time zone,
    operator_value double precision,
    note text DEFAULT ''::text NOT NULL,
    rule text NOT NULL,
    rule_version integer NOT NULL,
    evidence jsonb DEFAULT '[]'::jsonb NOT NULL,
    bounds jsonb DEFAULT '{}'::jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT config_derived_setting_key_check CHECK ((key ~ '^[a-z][a-z0-9_.]{0,127}$'::text)),
    CONSTRAINT config_derived_setting_pin CHECK (((pinned_value IS NULL) = (pinned_at IS NULL))),
    CONSTRAINT config_derived_setting_rule_version_check CHECK ((rule_version >= 1)),
    CONSTRAINT config_derived_setting_shadow CHECK (((shadow_value IS NULL) = (shadow_since IS NULL))),
    CONSTRAINT config_derived_setting_shadow_outcome_check CHECK ((shadow_outcome = ANY (ARRAY[''::text, 'not_worse'::text, 'worse'::text, 'insufficient'::text]))),
    CONSTRAINT config_derived_setting_status CHECK ((((status <> 'pinned'::text) OR (pinned_value IS NOT NULL)) AND ((status <> 'shadow'::text) OR (shadow_value IS NOT NULL)) AND ((status <> 'operator'::text) OR (operator_value IS NOT NULL)))),
    CONSTRAINT config_derived_setting_status_check CHECK ((status = ANY (ARRAY['default'::text, 'derived'::text, 'shadow'::text, 'pinned'::text, 'operator'::text])))
);


--
-- Name: crypto_meta; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.crypto_meta (
    id integer DEFAULT 1 NOT NULL,
    kdf_salt bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT crypto_meta_singleton CHECK ((id = 1))
);


--
-- Name: databases; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.databases (
    id integer NOT NULL,
    name text NOT NULL,
    host text NOT NULL,
    port integer DEFAULT 5432 NOT NULL,
    database_name text NOT NULL,
    username text NOT NULL,
    password_enc bytea NOT NULL,
    sslmode text DEFAULT 'require'::text NOT NULL,
    max_connections integer DEFAULT 2 NOT NULL,
    enabled boolean DEFAULT true,
    tags jsonb DEFAULT '{}'::jsonb,
    trust_level text DEFAULT 'observation'::text NOT NULL,
    execution_mode text DEFAULT 'approval'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    created_by integer,
    updated_at timestamp with time zone DEFAULT now()
);


--
-- Name: databases_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.databases_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: databases_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.databases_id_seq OWNED BY sage.databases.id;


--
-- Name: decision; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.decision (
    id bigint NOT NULL,
    database_id bigint,
    feature text NOT NULL,
    intent text NOT NULL,
    target_objects jsonb DEFAULT '[]'::jsonb NOT NULL,
    policy_id bigint,
    policy_version integer,
    verdict text NOT NULL,
    risk_tier text NOT NULL,
    reason text NOT NULL,
    guardrails jsonb DEFAULT '[]'::jsonb NOT NULL,
    off_window_ok boolean DEFAULT false NOT NULL,
    deadline_kind text,
    deadline_hard_at timestamp with time zone,
    evidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    evidence_id text NOT NULL,
    action_log_id bigint,
    queue_id integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    fingerprint text,
    repeat_count integer DEFAULT 1 NOT NULL,
    last_seen_at timestamp with time zone,
    principal_id text,
    task_id text,
    artifact_hash text,
    CONSTRAINT decision_risk_tier_check CHECK ((risk_tier = ANY (ARRAY['read_only'::text, 'safe'::text, 'moderate'::text, 'high'::text]))),
    CONSTRAINT decision_verdict_check CHECK ((verdict = ANY (ARRAY['execute'::text, 'queue_approval'::text, 'parked'::text, 'blocked'::text, 'observe_only'::text, 'recommended'::text])))
)
WITH (fillfactor='90');


--
-- Name: decision_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.decision_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: decision_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.decision_id_seq OWNED BY sage.decision.id;


--
-- Name: explain_cache; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.explain_cache (
    id bigint NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    queryid bigint NOT NULL,
    query_text text,
    plan_json jsonb NOT NULL,
    source text NOT NULL,
    total_cost double precision,
    execution_time double precision,
    plan_hash text,
    provider_scope_hash text DEFAULT ''::text NOT NULL,
    provider_compute_id text DEFAULT ''::text NOT NULL,
    provider_event_id text,
    comparison_key text,
    received_at timestamp with time zone,
    analyze_enabled boolean,
    buffers_enabled boolean,
    timing_enabled boolean,
    provider_plan_fingerprint text,
    duration_source text DEFAULT ''::text NOT NULL
);


--
-- Name: explain_cache_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.explain_cache_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: explain_cache_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.explain_cache_id_seq OWNED BY sage.explain_cache.id;


--
-- Name: explain_results; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.explain_results (
    query_hash bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    plan_json jsonb NOT NULL,
    explanation jsonb NOT NULL,
    database_name text DEFAULT ''::text NOT NULL
);


--
-- Name: fact_card_deliveries; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.fact_card_deliveries (
    id bigint NOT NULL,
    token_sha256 bytea NOT NULL,
    channel_id integer NOT NULL,
    database_name text NOT NULL,
    fact_id bigint NOT NULL,
    fact_hash text NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone,
    used_by integer,
    decision text,
    message_id bigint DEFAULT 0 NOT NULL,
    CONSTRAINT fact_card_deliveries_channel_id_check CHECK ((channel_id > 0)),
    CONSTRAINT fact_card_deliveries_check CHECK (((used_at IS NULL) = (decision IS NULL))),
    CONSTRAINT fact_card_deliveries_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 128))),
    CONSTRAINT fact_card_deliveries_decision_check CHECK ((decision = ANY (ARRAY['confirm'::text, 'reject'::text]))),
    CONSTRAINT fact_card_deliveries_fact_hash_check CHECK (((length(fact_hash) >= 1) AND (length(fact_hash) <= 128))),
    CONSTRAINT fact_card_deliveries_fact_id_check CHECK ((fact_id > 0)),
    CONSTRAINT fact_card_deliveries_title_check CHECK ((length(title) <= 512)),
    CONSTRAINT fact_card_deliveries_token_sha256_check CHECK ((octet_length(token_sha256) = 32))
);


--
-- Name: fact_card_deliveries_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.fact_card_deliveries_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: fact_card_deliveries_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.fact_card_deliveries_id_seq OWNED BY sage.fact_card_deliveries.id;


--
-- Name: facts; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.facts (
    id bigint NOT NULL,
    fact_type text NOT NULL,
    subject_kind text NOT NULL,
    subject text NOT NULL,
    value jsonb DEFAULT '{}'::jsonb NOT NULL,
    source text NOT NULL,
    proposed_by text DEFAULT ''::text NOT NULL,
    evidence jsonb DEFAULT '[]'::jsonb NOT NULL,
    rationale text DEFAULT ''::text NOT NULL,
    status text DEFAULT 'proposed'::text NOT NULL,
    decided_by text,
    decided_at timestamp with time zone,
    decision_note text DEFAULT ''::text NOT NULL,
    expires_at timestamp with time zone,
    expired_reason text DEFAULT ''::text NOT NULL,
    proposals integer DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_verified_at timestamp with time zone,
    subject_relid oid,
    subject_attnum smallint,
    CONSTRAINT facts_check CHECK (((status <> ALL (ARRAY['confirmed'::text, 'rejected'::text])) OR ((decided_by IS NOT NULL) AND (decided_at IS NOT NULL)))),
    CONSTRAINT facts_column_subject_v1 CHECK (((fact_type <> 'column_class'::text) OR ((subject_relid IS NOT NULL) AND (((subject_kind = 'column'::text) AND (subject_attnum > 0)) OR ((subject_kind = 'table'::text) AND (subject_attnum = 0)))))),
    CONSTRAINT facts_decided_by_check CHECK ((length(decided_by) <= 200)),
    CONSTRAINT facts_decision_note_check CHECK ((length(decision_note) <= 1000)),
    CONSTRAINT facts_expired_reason_check CHECK ((length(expired_reason) <= 500)),
    CONSTRAINT facts_fact_type_v2 CHECK ((fact_type = ANY (ARRAY['owned_by_app_migrations'::text, 'test_fixture'::text, 'slot_consumer'::text, 'append_only'::text, 'table_window'::text, 'column_class'::text]))),
    CONSTRAINT facts_proposals_check CHECK ((proposals >= 1)),
    CONSTRAINT facts_proposed_by_check CHECK ((length(proposed_by) <= 200)),
    CONSTRAINT facts_rationale_check CHECK ((length(rationale) <= 2000)),
    CONSTRAINT facts_source_check CHECK ((source = ANY (ARRAY['model'::text, 'operator'::text, 'detector'::text]))),
    CONSTRAINT facts_status_check CHECK ((status = ANY (ARRAY['proposed'::text, 'confirmed'::text, 'rejected'::text, 'expired'::text]))),
    CONSTRAINT facts_subject_check CHECK (((length(subject) >= 1) AND (length(subject) <= 300))),
    CONSTRAINT facts_subject_kind_v2 CHECK ((subject_kind = ANY (ARRAY['index'::text, 'table'::text, 'schema'::text, 'slot'::text, 'column'::text])))
);


--
-- Name: facts_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.facts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: facts_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.facts_id_seq OWNED BY sage.facts.id;


--
-- Name: findings; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.findings (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen timestamp with time zone DEFAULT now() NOT NULL,
    occurrence_count integer DEFAULT 1 NOT NULL,
    category text NOT NULL,
    severity text NOT NULL,
    object_type text,
    object_identifier text,
    title text NOT NULL,
    detail jsonb NOT NULL,
    recommendation text,
    recommended_sql text,
    rollback_sql text,
    estimated_cost_usd numeric(10,2),
    status text DEFAULT 'open'::text NOT NULL,
    suppressed_until timestamp with time zone,
    resolved_at timestamp with time zone,
    acted_on_at timestamp with time zone,
    action_log_id bigint,
    rule_id text,
    impact_score real
)
WITH (fillfactor='80');


--
-- Name: findings_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.findings_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: findings_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.findings_id_seq OWNED BY sage.findings.id;


--
-- Name: first_look; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.first_look (
    id bigint NOT NULL,
    database_name text NOT NULL,
    provider text DEFAULT ''::text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    finished_at timestamp with time zone NOT NULL,
    duration_ms bigint DEFAULT 0 NOT NULL,
    relations integer DEFAULT 0 NOT NULL,
    statement_timeout_ms integer DEFAULT 0 NOT NULL,
    items jsonb DEFAULT '[]'::jsonb NOT NULL,
    checks jsonb DEFAULT '[]'::jsonb NOT NULL,
    capabilities jsonb DEFAULT '[]'::jsonb NOT NULL,
    summary text DEFAULT ''::text NOT NULL,
    summary_model text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT first_look_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 128))),
    CONSTRAINT first_look_duration_ms_check CHECK ((duration_ms >= 0)),
    CONSTRAINT first_look_provider_check CHECK ((length(provider) <= 64)),
    CONSTRAINT first_look_relations_check CHECK ((relations >= 0)),
    CONSTRAINT first_look_statement_timeout_ms_check CHECK ((statement_timeout_ms >= 0)),
    CONSTRAINT first_look_summary_check CHECK ((length(summary) <= 2000)),
    CONSTRAINT first_look_summary_model_check CHECK ((length(summary_model) <= 200))
);


--
-- Name: first_look_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.first_look_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: first_look_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.first_look_id_seq OWNED BY sage.first_look.id;


--
-- Name: fleet_fingerprint; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.fleet_fingerprint (
    fleet_scope text NOT NULL,
    database_name text NOT NULL,
    boundary text DEFAULT ''::text NOT NULL,
    table_shapes text[] DEFAULT '{}'::text[] NOT NULL,
    index_shapes text[] DEFAULT '{}'::text[] NOT NULL,
    query_shapes text[] DEFAULT '{}'::text[] NOT NULL,
    table_labels jsonb,
    computed_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: fleet_leader_lease; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.fleet_leader_lease (
    scope text NOT NULL,
    holder text NOT NULL,
    epoch bigint NOT NULL,
    acquired_at timestamp with time zone DEFAULT now() NOT NULL,
    renewed_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT fleet_leader_lease_epoch_check CHECK ((epoch > 0))
);


--
-- Name: fleet_outcome_digest; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.fleet_outcome_digest (
    fleet_scope text NOT NULL,
    database_name text NOT NULL,
    action_class text NOT NULL,
    shape text DEFAULT ''::text NOT NULL,
    improved integer DEFAULT 0 NOT NULL,
    neutral integer DEFAULT 0 NOT NULL,
    regressed integer DEFAULT 0 NOT NULL,
    computed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT fleet_outcome_digest_improved_check CHECK ((improved >= 0)),
    CONSTRAINT fleet_outcome_digest_neutral_check CHECK ((neutral >= 0)),
    CONSTRAINT fleet_outcome_digest_regressed_check CHECK ((regressed >= 0))
);


--
-- Name: guard_cluster_roles; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_cluster_roles (
    principal_id text NOT NULL,
    cluster_key text NOT NULL,
    login_role text NOT NULL,
    broker_role text NOT NULL,
    broker_secret_ct bytea NOT NULL,
    key_id text NOT NULL,
    valid_until timestamp with time zone,
    prior_attrs jsonb,
    status text DEFAULT 'active'::text NOT NULL,
    rotated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT guard_cluster_roles_broker_role_check CHECK ((broker_role ~ '^sage_agentb_[a-z2-7]{10}$'::text)),
    CONSTRAINT guard_cluster_roles_cluster_key_check CHECK (((length(cluster_key) >= 1) AND (length(cluster_key) <= 500))),
    CONSTRAINT guard_cluster_roles_login_role_check CHECK ((login_role ~ '^sage_agent_[a-z2-7]{10}$'::text)),
    CONSTRAINT guard_cluster_roles_status_check CHECK ((status = ANY (ARRAY['active'::text, 'killed'::text, 'retired'::text])))
);


--
-- Name: guard_environment_labels; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_environment_labels (
    database_id uuid NOT NULL,
    label text DEFAULT 'prod'::text NOT NULL,
    identity jsonb NOT NULL,
    verified boolean DEFAULT false NOT NULL,
    set_by text NOT NULL,
    set_at timestamp with time zone DEFAULT now() NOT NULL,
    observed jsonb,
    observed_at timestamp with time zone,
    pending_label text,
    pending_by text,
    pending_at timestamp with time zone,
    CONSTRAINT guard_environment_labels_check CHECK ((((pending_label IS NULL) = (pending_by IS NULL)) AND ((pending_label IS NULL) = (pending_at IS NULL)))),
    CONSTRAINT guard_environment_labels_label_check CHECK ((label = ANY (ARRAY['branch'::text, 'dev'::text, 'stage'::text, 'prod'::text]))),
    CONSTRAINT guard_environment_labels_pending_by_check CHECK (((length(pending_by) >= 1) AND (length(pending_by) <= 200))),
    CONSTRAINT guard_environment_labels_pending_label_check CHECK ((pending_label = ANY (ARRAY['branch'::text, 'dev'::text, 'stage'::text, 'prod'::text]))),
    CONSTRAINT guard_environment_labels_set_by_check CHECK (((length(set_by) >= 1) AND (length(set_by) <= 200)))
);


--
-- Name: guard_freezes; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_freezes (
    id bigint NOT NULL,
    scope text NOT NULL,
    target text DEFAULT ''::text NOT NULL,
    kill_id bigint,
    reason text NOT NULL,
    set_by text NOT NULL,
    set_at timestamp with time zone DEFAULT now() NOT NULL,
    cleared_at timestamp with time zone,
    cleared_by text,
    CONSTRAINT guard_freezes_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 2000))),
    CONSTRAINT guard_freezes_scope_check CHECK ((scope = ANY (ARRAY['principal'::text, 'database'::text, 'fleet'::text]))),
    CONSTRAINT guard_freezes_set_by_check CHECK (((length(set_by) >= 1) AND (length(set_by) <= 200))),
    CONSTRAINT guard_freezes_target_check CHECK ((length(target) <= 200))
);


--
-- Name: guard_freezes_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.guard_freezes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: guard_freezes_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.guard_freezes_id_seq OWNED BY sage.guard_freezes.id;


--
-- Name: guard_grant_requests; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_grant_requests (
    id bigint NOT NULL,
    database_id uuid NOT NULL,
    principal_id text NOT NULL,
    capability text NOT NULL,
    objects jsonb NOT NULL,
    duration_minutes integer NOT NULL,
    reason text NOT NULL,
    task_id text,
    status text DEFAULT 'pending'::text NOT NULL,
    reason_code text,
    detail text,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    decided_by integer,
    decided_at timestamp with time zone,
    grant_ids bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    CONSTRAINT guard_grant_requests_capability_check CHECK ((capability ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT guard_grant_requests_detail_check CHECK ((length(detail) <= 4000)),
    CONSTRAINT guard_grant_requests_duration_minutes_check CHECK ((duration_minutes > 0)),
    CONSTRAINT guard_grant_requests_principal_id_check CHECK ((principal_id ~ '^agp_[a-z2-7]{20}$'::text)),
    CONSTRAINT guard_grant_requests_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 2000))),
    CONSTRAINT guard_grant_requests_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'denied'::text, 'expired'::text, 'failed'::text, 'recorded'::text])))
);


--
-- Name: guard_grant_requests_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.guard_grant_requests_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: guard_grant_requests_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.guard_grant_requests_id_seq OWNED BY sage.guard_grant_requests.id;


--
-- Name: guard_grants; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_grants (
    id bigint NOT NULL,
    database_id uuid NOT NULL,
    principal_id text NOT NULL,
    lane text NOT NULL,
    capability text NOT NULL,
    object_oid oid NOT NULL,
    object_name text NOT NULL,
    columns text[] NOT NULL,
    privileges text[] NOT NULL,
    grantor text NOT NULL,
    granted_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    state text DEFAULT 'active'::text NOT NULL,
    grant_action_id bigint NOT NULL,
    revoke_action_id bigint,
    object_kind text DEFAULT 'relation'::text NOT NULL,
    schema_oid oid,
    revoke_detail text,
    CONSTRAINT guard_grants_capability_check CHECK ((capability ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT guard_grants_check CHECK ((expires_at > granted_at)),
    CONSTRAINT guard_grants_lane_check CHECK ((lane = ANY (ARRAY['direct'::text, 'broker'::text]))),
    CONSTRAINT guard_grants_object_kind_check CHECK ((object_kind = ANY (ARRAY['relation'::text, 'schema'::text]))),
    CONSTRAINT guard_grants_principal_id_check CHECK ((principal_id ~ '^agp_[a-z2-7]{20}$'::text)),
    CONSTRAINT guard_grants_revoke_detail_check CHECK ((length(revoke_detail) <= 4000)),
    CONSTRAINT guard_grants_state_check CHECK ((state = ANY (ARRAY['active'::text, 'revoked'::text, 'revoke_incomplete'::text])))
);


--
-- Name: guard_grants_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.guard_grants_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: guard_grants_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.guard_grants_id_seq OWNED BY sage.guard_grants.id;


--
-- Name: guard_identity_bindings; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_identity_bindings (
    issuer text NOT NULL,
    subject text NOT NULL,
    principal_id text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: guard_inflight; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_inflight (
    principal_id text NOT NULL,
    database_id uuid NOT NULL,
    backend_pid integer NOT NULL,
    action_id bigint,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT guard_inflight_backend_pid_check CHECK ((backend_pid > 0)),
    CONSTRAINT guard_inflight_principal_id_check CHECK ((principal_id ~ '^agp_[a-z2-7]{20}$'::text))
);


--
-- Name: guard_kills; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_kills (
    id bigint NOT NULL,
    scope text NOT NULL,
    target text DEFAULT ''::text NOT NULL,
    reason text NOT NULL,
    requested_by text NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    report jsonb,
    CONSTRAINT guard_kills_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 2000))),
    CONSTRAINT guard_kills_requested_by_check CHECK (((length(requested_by) >= 1) AND (length(requested_by) <= 200))),
    CONSTRAINT guard_kills_scope_check CHECK ((scope = ANY (ARRAY['all'::text, 'principal'::text, 'database'::text]))),
    CONSTRAINT guard_kills_target_check CHECK ((length(target) <= 200))
);


--
-- Name: guard_kills_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.guard_kills_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: guard_kills_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.guard_kills_id_seq OWNED BY sage.guard_kills.id;


--
-- Name: guard_pgaudit_events; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_pgaudit_events (
    id bigint NOT NULL,
    database_name text NOT NULL,
    logged_at timestamp with time zone NOT NULL,
    session_id text DEFAULT ''::text NOT NULL,
    pid integer DEFAULT 0 NOT NULL,
    db_user text DEFAULT ''::text NOT NULL,
    application text DEFAULT ''::text NOT NULL,
    principal_id text,
    action_id bigint,
    audit_type text NOT NULL,
    statement_id bigint DEFAULT 0 NOT NULL,
    substatement_id bigint DEFAULT 0 NOT NULL,
    class text DEFAULT ''::text NOT NULL,
    command text DEFAULT ''::text NOT NULL,
    object_type text DEFAULT ''::text NOT NULL,
    object_name text DEFAULT ''::text NOT NULL,
    statement text DEFAULT ''::text NOT NULL,
    correlated_by text NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    chain_seq bigint,
    chain_prev_hash text,
    chain_hash text
);


--
-- Name: guard_pgaudit_events_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.guard_pgaudit_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: guard_pgaudit_events_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.guard_pgaudit_events_id_seq OWNED BY sage.guard_pgaudit_events.id;


--
-- Name: guard_principal_files; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_principal_files (
    principal_id text NOT NULL,
    file_name text NOT NULL,
    applied_by text NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT guard_principal_files_file_name_check CHECK ((file_name ~ '^[a-z][a-z0-9-]{1,62}$'::text))
);


--
-- Name: guard_principals; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_principals (
    id text NOT NULL,
    name text NOT NULL,
    sponsor_user_id integer,
    tenant text DEFAULT ''::text NOT NULL,
    profile text NOT NULL,
    env_ceiling text DEFAULT 'dev'::text NOT NULL,
    status text DEFAULT 'active'::text NOT NULL,
    frozen_reason text DEFAULT ''::text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_by integer,
    CONSTRAINT guard_principals_created_by_check CHECK (((length(created_by) >= 1) AND (length(created_by) <= 200))),
    CONSTRAINT guard_principals_env_ceiling_check CHECK ((env_ceiling = ANY (ARRAY['branch'::text, 'dev'::text, 'stage'::text, 'prod'::text]))),
    CONSTRAINT guard_principals_frozen_reason_check CHECK ((length(frozen_reason) <= 2000)),
    CONSTRAINT guard_principals_id_check CHECK ((id ~ '^agp_[a-z2-7]{20}$'::text)),
    CONSTRAINT guard_principals_name_check CHECK ((name ~ '^[a-z][a-z0-9-]{1,62}$'::text)),
    CONSTRAINT guard_principals_profile_check CHECK (((length(profile) >= 1) AND (length(profile) <= 100))),
    CONSTRAINT guard_principals_status_check CHECK ((status = ANY (ARRAY['active'::text, 'frozen'::text, 'retired'::text]))),
    CONSTRAINT guard_principals_tenant_check CHECK ((length(tenant) <= 200))
);


--
-- Name: guard_public_baseline; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_public_baseline (
    object_kind text NOT NULL,
    object_oid oid NOT NULL,
    attnum smallint DEFAULT 0 NOT NULL,
    object_name text NOT NULL,
    privilege text NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT guard_public_baseline_object_kind_check CHECK ((object_kind = ANY (ARRAY['database'::text, 'schema'::text, 'relation'::text, 'column'::text, 'function'::text, 'default_acl'::text])))
);


--
-- Name: guard_query_audit; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_query_audit (
    id bigint NOT NULL,
    database_id uuid NOT NULL,
    principal_id text NOT NULL,
    task_id text,
    envelope_hash text NOT NULL,
    verdict text NOT NULL,
    reason text,
    step text,
    row_count integer,
    classes text[] DEFAULT '{}'::text[] NOT NULL,
    fingerprint text,
    broker_role text,
    pss_dealloc bigint,
    pss_reset timestamp with time zone,
    at timestamp with time zone DEFAULT now() NOT NULL,
    chain_seq bigint,
    chain_prev_hash text,
    chain_hash text,
    CONSTRAINT guard_query_audit_broker_role_check CHECK ((length(broker_role) <= 63)),
    CONSTRAINT guard_query_audit_envelope_hash_check CHECK (((length(envelope_hash) >= 1) AND (length(envelope_hash) <= 128))),
    CONSTRAINT guard_query_audit_fingerprint_check CHECK ((length(fingerprint) <= 64)),
    CONSTRAINT guard_query_audit_principal_id_check CHECK (((length(principal_id) >= 1) AND (length(principal_id) <= 100))),
    CONSTRAINT guard_query_audit_reason_check CHECK ((length(reason) <= 100)),
    CONSTRAINT guard_query_audit_row_count_check CHECK ((row_count >= 0)),
    CONSTRAINT guard_query_audit_step_check CHECK ((length(step) <= 10)),
    CONSTRAINT guard_query_audit_task_id_check CHECK ((length(task_id) <= 200)),
    CONSTRAINT guard_query_audit_verdict_check CHECK ((verdict = ANY (ARRAY['execute'::text, 'blocked'::text])))
);


--
-- Name: guard_query_audit_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.guard_query_audit_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: guard_query_audit_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.guard_query_audit_id_seq OWNED BY sage.guard_query_audit.id;


--
-- Name: guard_taint; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_taint (
    principal_id text NOT NULL,
    task_id text DEFAULT '*'::text NOT NULL,
    source text NOT NULL,
    tainted_at timestamp with time zone DEFAULT now() NOT NULL,
    cleared_at timestamp with time zone,
    cleared_by text,
    CONSTRAINT guard_taint_source_check CHECK (((length(source) >= 1) AND (length(source) <= 2000)))
);


--
-- Name: guard_unfreeze_requests; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.guard_unfreeze_requests (
    id bigint NOT NULL,
    scope text NOT NULL,
    target text DEFAULT ''::text NOT NULL,
    freeze_id bigint NOT NULL,
    requested_by text NOT NULL,
    requested_by_user integer NOT NULL,
    reason text NOT NULL,
    requested_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    approved_by text,
    approved_by_user integer,
    decided_at timestamp with time zone,
    status text DEFAULT 'pending'::text NOT NULL,
    CONSTRAINT guard_unfreeze_requests_reason_check CHECK ((length(reason) <= 2000)),
    CONSTRAINT guard_unfreeze_requests_requested_by_check CHECK (((length(requested_by) >= 1) AND (length(requested_by) <= 200))),
    CONSTRAINT guard_unfreeze_requests_scope_check CHECK ((scope = ANY (ARRAY['principal'::text, 'database'::text, 'fleet'::text]))),
    CONSTRAINT guard_unfreeze_requests_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'applied'::text, 'expired'::text, 'superseded'::text]))),
    CONSTRAINT guard_unfreeze_requests_target_check CHECK ((length(target) <= 200))
);


--
-- Name: guard_unfreeze_requests_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.guard_unfreeze_requests_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: guard_unfreeze_requests_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.guard_unfreeze_requests_id_seq OWNED BY sage.guard_unfreeze_requests.id;


--
-- Name: ha_identity; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.ha_identity (
    monitor_key text NOT NULL,
    role text NOT NULL,
    timeline_id bigint,
    system_identifier text,
    server_started_at timestamp with time zone,
    last_change_at timestamp with time zone,
    observed_at timestamp with time zone NOT NULL,
    CONSTRAINT ha_identity_monitor_key_check CHECK (((length(monitor_key) >= 1) AND (length(monitor_key) <= 200))),
    CONSTRAINT ha_identity_role_check CHECK ((role = ANY (ARRAY['primary'::text, 'replica'::text]))),
    CONSTRAINT ha_identity_system_identifier_check CHECK (((length(system_identifier) >= 1) AND (length(system_identifier) <= 32))),
    CONSTRAINT ha_identity_timeline_id_check CHECK ((timeline_id > 0))
);


--
-- Name: health_history; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.health_history (
    id bigint NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    database_name text NOT NULL,
    health_score integer NOT NULL,
    findings_open integer DEFAULT 0 NOT NULL,
    findings_critical integer DEFAULT 0 NOT NULL,
    findings_warning integer DEFAULT 0 NOT NULL,
    findings_info integer DEFAULT 0 NOT NULL,
    actions_total integer DEFAULT 0 NOT NULL
);


--
-- Name: health_history_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.health_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: health_history_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.health_history_id_seq OWNED BY sage.health_history.id;


--
-- Name: incident_avoided; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.incident_avoided (
    id bigint NOT NULL,
    database_id bigint,
    kind text NOT NULL,
    severity text NOT NULL,
    credited_minutes numeric NOT NULL,
    evidence_id text NOT NULL,
    model_version integer DEFAULT 1 NOT NULL,
    decision_id bigint NOT NULL,
    action_log_id bigint NOT NULL,
    verification_id bigint NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT incident_avoided_credited_minutes_check CHECK ((credited_minutes >= (0)::numeric)),
    CONSTRAINT incident_avoided_kind_check CHECK ((kind = ANY (ARRAY['xid_wraparound'::text, 'disk_full_slot'::text, 'lock_storm'::text]))),
    CONSTRAINT incident_avoided_severity_check CHECK ((severity = ANY (ARRAY['near_miss'::text, 'prevented'::text])))
);


--
-- Name: incident_avoided_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.incident_avoided_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: incident_avoided_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.incident_avoided_id_seq OWNED BY sage.incident_avoided.id;


--
-- Name: incidents; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.incidents (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    detected_at timestamp with time zone DEFAULT now() NOT NULL,
    last_detected_at timestamp with time zone DEFAULT now() NOT NULL,
    severity text NOT NULL,
    root_cause text NOT NULL,
    causal_chain jsonb DEFAULT '[]'::jsonb NOT NULL,
    affected_objects text[] DEFAULT '{}'::text[] NOT NULL,
    signal_ids text[] DEFAULT '{}'::text[] NOT NULL,
    recommended_sql text,
    rollback_sql text,
    action_risk text,
    source text NOT NULL,
    confidence numeric(3,2) DEFAULT 1.0 NOT NULL,
    related_findings uuid[],
    resolved_at timestamp with time zone,
    database_name text,
    occurrence_count integer DEFAULT 1 NOT NULL,
    escalated_at timestamp with time zone,
    identity_key text,
    resolved_by text,
    resolution_reason text,
    previous_incident_id uuid,
    CONSTRAINT incidents_action_risk_check CHECK (((action_risk = ANY (ARRAY['safe'::text, 'moderate'::text, 'high_risk'::text, 'low'::text, 'medium'::text, 'high'::text])) OR (action_risk IS NULL))),
    CONSTRAINT incidents_severity_check CHECK ((severity = ANY (ARRAY['info'::text, 'warning'::text, 'critical'::text]))),
    CONSTRAINT incidents_source_check CHECK ((source = ANY (ARRAY['deterministic'::text, 'log_deterministic'::text, 'self_action'::text, 'manual_review_required'::text, 'llm'::text, 'schema_advisor'::text, 'schema_lint'::text, 'n_plus_one'::text])))
)
WITH (fillfactor='80', autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: index_replace; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.index_replace (
    id bigint NOT NULL,
    database_id bigint,
    finding_id bigint,
    action_log_id bigint,
    decision_id bigint,
    approved_by integer,
    table_name text NOT NULL,
    new_index text NOT NULL,
    new_index_oid bigint,
    create_sql text NOT NULL,
    drop_sql text NOT NULL,
    old_index text NOT NULL,
    old_index_oid bigint NOT NULL,
    old_definition text NOT NULL,
    rollback_sql text NOT NULL,
    state text NOT NULL,
    verify_phase text DEFAULT 'none'::text NOT NULL,
    before_state jsonb DEFAULT '{}'::jsonb NOT NULL,
    error text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT index_replace_phase_check CHECK ((verify_phase = ANY (ARRAY['none'::text, 'judging'::text, 'watch'::text, 'done'::text]))),
    CONSTRAINT index_replace_state_check CHECK ((state = ANY (ARRAY['creating'::text, 'created'::text, 'dropping'::text, 'completed'::text, 'create_failed'::text, 'drop_failed'::text, 'rollback_recreating'::text, 'rollback_dropping'::text, 'rolled_back'::text, 'rollback_failed'::text, 'old_restoring'::text, 'old_restored'::text])))
);


--
-- Name: index_replace_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.index_replace_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: index_replace_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.index_replace_id_seq OWNED BY sage.index_replace.id;


--
-- Name: io_rate_sample; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.io_rate_sample (
    id bigint NOT NULL,
    database_name text NOT NULL,
    sampled_at timestamp with time zone NOT NULL,
    interval_seconds double precision NOT NULL,
    data_bytes_per_sec double precision NOT NULL,
    wal_bytes_per_sec double precision NOT NULL,
    source text NOT NULL,
    CONSTRAINT io_rate_sample_data_bytes_per_sec_check CHECK ((data_bytes_per_sec >= (0)::double precision)),
    CONSTRAINT io_rate_sample_interval_seconds_check CHECK ((interval_seconds > (0)::double precision)),
    CONSTRAINT io_rate_sample_wal_bytes_per_sec_check CHECK ((wal_bytes_per_sec >= (0)::double precision))
);


--
-- Name: io_rate_sample_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.io_rate_sample_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: io_rate_sample_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.io_rate_sample_id_seq OWNED BY sage.io_rate_sample.id;


--
-- Name: lease_queue; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.lease_queue (
    id bigint NOT NULL,
    database_id bigint,
    request_key text NOT NULL,
    object_keys text[] NOT NULL,
    kind text NOT NULL,
    actor text NOT NULL,
    intent text NOT NULL,
    decision_id bigint,
    instance text NOT NULL,
    state text DEFAULT 'waiting'::text NOT NULL,
    enqueued_at timestamp with time zone DEFAULT now() NOT NULL,
    heartbeat_at timestamp with time zone DEFAULT now() NOT NULL,
    deadline_at timestamp with time zone NOT NULL,
    resolved_at timestamp with time zone,
    resumed_count integer DEFAULT 0 NOT NULL,
    CONSTRAINT lease_queue_state_check CHECK ((state = ANY (ARRAY['waiting'::text, 'granted'::text, 'timed_out'::text, 'cancelled'::text])))
);


--
-- Name: lease_queue_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.lease_queue_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: lease_queue_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.lease_queue_id_seq OWNED BY sage.lease_queue.id;


--
-- Name: managed_change_proposals; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.managed_change_proposals (
    id bigint NOT NULL,
    fingerprint text NOT NULL,
    provider text NOT NULL,
    mechanism text NOT NULL,
    target text NOT NULL,
    parameter text NOT NULL,
    value text NOT NULL,
    reboot_required boolean NOT NULL,
    proposal jsonb NOT NULL,
    finding_id bigint,
    status text DEFAULT 'pending'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    decided_by integer,
    decided_at timestamp with time zone,
    decision_note text DEFAULT ''::text NOT NULL,
    applied_at timestamp with time zone,
    CONSTRAINT managed_change_proposals_fingerprint_check CHECK (((length(fingerprint) >= 1) AND (length(fingerprint) <= 64))),
    CONSTRAINT managed_change_proposals_provider_check CHECK ((provider = ANY (ARRAY['rds'::text, 'aurora'::text, 'cloud-sql'::text]))),
    CONSTRAINT managed_change_proposals_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'rejected'::text, 'applied'::text, 'superseded'::text])))
);


--
-- Name: managed_change_proposals_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.managed_change_proposals_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: managed_change_proposals_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.managed_change_proposals_id_seq OWNED BY sage.managed_change_proposals.id;


--
-- Name: mcp_tokens; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.mcp_tokens (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    scopes text[] NOT NULL,
    databases text[] NOT NULL,
    token_hash text NOT NULL,
    prefix text NOT NULL,
    owner_user_id integer,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    revoked_by text,
    last_used_at timestamp with time zone,
    principal_id text,
    CONSTRAINT mcp_tokens_check CHECK (((kind = 'operator'::text) OR (NOT ('approve'::text = ANY (scopes))))),
    CONSTRAINT mcp_tokens_check1 CHECK (((kind <> 'operator'::text) OR (owner_user_id IS NOT NULL))),
    CONSTRAINT mcp_tokens_check2 CHECK (((kind <> 'agent'::text) OR (owner_user_id IS NULL))),
    CONSTRAINT mcp_tokens_check3 CHECK ((expires_at > created_at)),
    CONSTRAINT mcp_tokens_created_by_check CHECK (((length(created_by) >= 1) AND (length(created_by) <= 200))),
    CONSTRAINT mcp_tokens_databases_check CHECK ((cardinality(databases) >= 1)),
    CONSTRAINT mcp_tokens_kind_check CHECK ((kind = ANY (ARRAY['agent'::text, 'operator'::text]))),
    CONSTRAINT mcp_tokens_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100))),
    CONSTRAINT mcp_tokens_prefix_check CHECK (((length(prefix) >= 1) AND (length(prefix) <= 16))),
    CONSTRAINT mcp_tokens_revoked_by_check CHECK ((length(revoked_by) <= 200)),
    CONSTRAINT mcp_tokens_scopes_check CHECK (((scopes <@ ARRAY['read'::text, 'propose'::text, 'approve'::text]) AND ('read'::text = ANY (scopes)))),
    CONSTRAINT mcp_tokens_token_hash_check CHECK (((length(token_hash) >= 32) AND (length(token_hash) <= 128)))
);


--
-- Name: migration_run; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.migration_run (
    id bigint NOT NULL,
    database_id bigint,
    evidence_id text NOT NULL,
    phase text NOT NULL,
    source_sql_hash text NOT NULL,
    clone_id text,
    clone_created_from timestamp with time zone,
    measurement jsonb DEFAULT '{}'::jsonb NOT NULL,
    verdict text NOT NULL,
    contract_not_before timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT migration_run_phase_check CHECK ((phase = ANY (ARRAY['plan'::text, 'rehearse'::text, 'expand'::text, 'verify'::text, 'contract'::text]))),
    CONSTRAINT migration_run_verdict_check CHECK ((verdict = ANY (ARRAY['pending'::text, 'recommend_only'::text, 'parked'::text, 'promote'::text, 'failed'::text])))
);


--
-- Name: migration_run_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.migration_run_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: migration_run_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.migration_run_id_seq OWNED BY sage.migration_run.id;


--
-- Name: notification_channels; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.notification_channels (
    id integer NOT NULL,
    name text NOT NULL,
    type text NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    enabled boolean DEFAULT true,
    created_at timestamp with time zone DEFAULT now(),
    created_by integer
);


--
-- Name: notification_channels_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.notification_channels_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: notification_channels_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.notification_channels_id_seq OWNED BY sage.notification_channels.id;


--
-- Name: notification_log; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.notification_log (
    id integer NOT NULL,
    channel_id integer,
    event text NOT NULL,
    subject text NOT NULL,
    body text,
    status text DEFAULT 'pending'::text NOT NULL,
    error text,
    sent_at timestamp with time zone DEFAULT now()
);


--
-- Name: notification_log_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.notification_log_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: notification_log_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.notification_log_id_seq OWNED BY sage.notification_log.id;


--
-- Name: notification_rules; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.notification_rules (
    id integer NOT NULL,
    channel_id integer,
    event text NOT NULL,
    min_severity text DEFAULT 'warning'::text,
    enabled boolean DEFAULT true,
    created_at timestamp with time zone DEFAULT now()
);


--
-- Name: notification_rules_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.notification_rules_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: notification_rules_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.notification_rules_id_seq OWNED BY sage.notification_rules.id;


--
-- Name: onboarding; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.onboarding (
    database_name text NOT NULL,
    install_kind text NOT NULL,
    installed_at timestamp with time zone DEFAULT now() NOT NULL,
    first_look_at timestamp with time zone,
    first_finding_at timestamp with time zone,
    first_finding_source text DEFAULT ''::text NOT NULL,
    ttff_ms bigint,
    CONSTRAINT onboarding_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 128))),
    CONSTRAINT onboarding_first_finding_source_check CHECK ((length(first_finding_source) <= 64)),
    CONSTRAINT onboarding_install_kind_check CHECK ((install_kind = ANY (ARRAY['new'::text, 'existing'::text]))),
    CONSTRAINT onboarding_ttff_ms_check CHECK ((ttff_ms >= 0))
);


--
-- Name: optimizer_rejection; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.optimizer_rejection (
    id bigint NOT NULL,
    database_name text DEFAULT current_database() NOT NULL,
    schema_name text NOT NULL,
    table_name text NOT NULL,
    shape_hash text NOT NULL,
    method text NOT NULL,
    key_cols text[] NOT NULL,
    predicate text DEFAULT ''::text NOT NULL,
    include_cols text[] DEFAULT '{}'::text[] NOT NULL,
    ddl text NOT NULL,
    improvement_pct double precision NOT NULL,
    min_improvement_pct double precision NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    workload jsonb DEFAULT '[]'::jsonb NOT NULL,
    row_estimate bigint DEFAULT 0 NOT NULL,
    measure_count integer DEFAULT 1 NOT NULL,
    first_measured_at timestamp with time zone DEFAULT now() NOT NULL,
    measured_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT optimizer_rejection_ddl_check CHECK ((length(ddl) <= 8192)),
    CONSTRAINT optimizer_rejection_key_cols_check CHECK ((cardinality(key_cols) >= 1)),
    CONSTRAINT optimizer_rejection_measure_count_check CHECK ((measure_count >= 1)),
    CONSTRAINT optimizer_rejection_method_check CHECK (((length(method) >= 1) AND (length(method) <= 64))),
    CONSTRAINT optimizer_rejection_reason_check CHECK ((length(reason) <= 1024)),
    CONSTRAINT optimizer_rejection_schema_name_check CHECK (((length(schema_name) >= 1) AND (length(schema_name) <= 128))),
    CONSTRAINT optimizer_rejection_shape_hash_check CHECK ((length(shape_hash) = 64)),
    CONSTRAINT optimizer_rejection_table_name_check CHECK (((length(table_name) >= 1) AND (length(table_name) <= 128))),
    CONSTRAINT optimizer_rejection_workload_check CHECK ((jsonb_typeof(workload) = 'array'::text))
);


--
-- Name: optimizer_rejection_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.optimizer_rejection_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: optimizer_rejection_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.optimizer_rejection_id_seq OWNED BY sage.optimizer_rejection.id;


--
-- Name: policy; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.policy (
    id bigint NOT NULL,
    database_id bigint,
    version integer NOT NULL,
    scope text DEFAULT 'database'::text NOT NULL,
    profile text DEFAULT 'staffed'::text NOT NULL,
    doc jsonb NOT NULL,
    preview jsonb DEFAULT '{"changed_pending_outcomes": []}'::jsonb NOT NULL,
    schema_version integer DEFAULT 3 NOT NULL,
    status text DEFAULT 'proposed'::text NOT NULL,
    proposed_by text NOT NULL,
    ratified_by text,
    proposed_at timestamp with time zone DEFAULT now() NOT NULL,
    ratified_at timestamp with time zone,
    activated_at timestamp with time zone,
    supersedes_id bigint,
    proposed_principal_id text,
    proposed_sponsor_id integer,
    widening boolean DEFAULT false NOT NULL,
    CONSTRAINT policy_profile_check CHECK ((profile = ANY (ARRAY['staffed'::text, 'unattended'::text]))),
    CONSTRAINT policy_status_check CHECK ((status = ANY (ARRAY['proposed'::text, 'ratified'::text, 'active'::text, 'superseded'::text, 'rejected'::text])))
);


--
-- Name: policy_approvals; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.policy_approvals (
    policy_id bigint NOT NULL,
    approver text NOT NULL,
    approver_user_id integer NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    single_operator boolean DEFAULT false NOT NULL,
    review_status text,
    decided_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT policy_approvals_approver_check CHECK (((length(approver) >= 1) AND (length(approver) <= 200))),
    CONSTRAINT policy_approvals_approver_user_id_check CHECK ((approver_user_id > 0)),
    CONSTRAINT policy_approvals_reason_check CHECK ((length(reason) <= 2000)),
    CONSTRAINT policy_approvals_review_status_check CHECK ((review_status = ANY (ARRAY['pending'::text, 'reviewed'::text])))
);


--
-- Name: policy_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.policy_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: policy_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.policy_id_seq OWNED BY sage.policy.id;


--
-- Name: query_hints; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.query_hints (
    id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    queryid bigint NOT NULL,
    hint_plan_id bigint,
    hint_text text NOT NULL,
    symptom text NOT NULL,
    before_cost double precision,
    after_cost double precision,
    status text DEFAULT 'active'::text NOT NULL,
    verified_at timestamp with time zone,
    rolled_back_at timestamp with time zone,
    suggested_rewrite text DEFAULT ''::text,
    rewrite_rationale text DEFAULT ''::text,
    calls_at_last_check bigint,
    last_revalidated_at timestamp with time zone
)
WITH (autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: query_hints_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.query_hints_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: query_hints_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.query_hints_id_seq OWNED BY sage.query_hints.id;


--
-- Name: query_store; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.query_store (
    id bigint NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    queryid bigint NOT NULL,
    calls bigint NOT NULL,
    total_exec_time double precision NOT NULL,
    mean_exec_time double precision NOT NULL,
    rows bigint DEFAULT 0 NOT NULL,
    plan_hash text,
    stats_epoch timestamp with time zone
)
PARTITION BY RANGE (captured_at);


--
-- Name: query_store_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.query_store_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: query_store_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.query_store_id_seq OWNED BY sage.query_store.id;


--
-- Name: query_store_default; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.query_store_default (
    id bigint DEFAULT nextval('sage.query_store_id_seq'::regclass) NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    queryid bigint NOT NULL,
    calls bigint NOT NULL,
    total_exec_time double precision NOT NULL,
    mean_exec_time double precision NOT NULL,
    rows bigint DEFAULT 0 NOT NULL,
    plan_hash text,
    stats_epoch timestamp with time zone
);


--
-- Name: query_store_history; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.query_store_history (
    id bigint DEFAULT nextval('sage.query_store_id_seq'::regclass) NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    queryid bigint NOT NULL,
    calls bigint NOT NULL,
    total_exec_time double precision NOT NULL,
    mean_exec_time double precision NOT NULL,
    rows bigint DEFAULT 0 NOT NULL,
    plan_hash text,
    stats_epoch timestamp with time zone
);


--
-- Name: query_store_p20261117; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.query_store_p20261117 (
    id bigint DEFAULT nextval('sage.query_store_id_seq'::regclass) NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    queryid bigint NOT NULL,
    calls bigint NOT NULL,
    total_exec_time double precision NOT NULL,
    mean_exec_time double precision NOT NULL,
    rows bigint DEFAULT 0 NOT NULL,
    plan_hash text,
    stats_epoch timestamp with time zone
);


--
-- Name: query_store_p20261118; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.query_store_p20261118 (
    id bigint DEFAULT nextval('sage.query_store_id_seq'::regclass) NOT NULL,
    captured_at timestamp with time zone DEFAULT now() NOT NULL,
    queryid bigint NOT NULL,
    calls bigint NOT NULL,
    total_exec_time double precision NOT NULL,
    mean_exec_time double precision NOT NULL,
    rows bigint DEFAULT 0 NOT NULL,
    plan_hash text,
    stats_epoch timestamp with time zone
);


--
-- Name: recommendation; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.recommendation (
    id bigint NOT NULL,
    identity_key text NOT NULL,
    database_name text NOT NULL,
    category text NOT NULL,
    target text NOT NULL,
    action_type text NOT NULL,
    index_fingerprint text DEFAULT ''::text NOT NULL,
    finding_id bigint,
    state text NOT NULL,
    revision integer NOT NULL,
    content_hash text NOT NULL,
    approved_revision integer,
    approved_hash text,
    approved_by text,
    approved_at timestamp with time zone,
    attempt_count integer DEFAULT 0 NOT NULL,
    retry_budget integer NOT NULL,
    next_attempt_at timestamp with time zone,
    lease_until timestamp with time zone,
    action_log_id bigint,
    verdict text,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT recommendation_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT recommendation_check CHECK (((approved_revision IS NULL) = (approved_hash IS NULL))),
    CONSTRAINT recommendation_check1 CHECK (((approved_hash IS NULL) = (approved_by IS NULL))),
    CONSTRAINT recommendation_check2 CHECK (((approved_revision IS NULL) OR (approved_revision <= revision))),
    CONSTRAINT recommendation_check3 CHECK (((state <> ALL (ARRAY['approved'::text, 'applying'::text, 'applied'::text, 'verifying'::text])) OR (approved_hash IS NOT NULL))),
    CONSTRAINT recommendation_identity_key_check CHECK (((length(identity_key) >= 1) AND (length(identity_key) <= 64))),
    CONSTRAINT recommendation_retry_budget_check CHECK ((retry_budget >= 0)),
    CONSTRAINT recommendation_revision_check CHECK ((revision > 0)),
    CONSTRAINT recommendation_state_check CHECK ((state = ANY (ARRAY['proposed'::text, 'approved'::text, 'applying'::text, 'applied'::text, 'verifying'::text, 'verified'::text, 'reverted'::text, 'inconclusive'::text, 'superseded'::text, 'failed'::text, 'abandoned'::text])))
)
WITH (fillfactor='90');


--
-- Name: recommendation_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.recommendation_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: recommendation_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.recommendation_id_seq OWNED BY sage.recommendation.id;


--
-- Name: recommendation_revision; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.recommendation_revision (
    recommendation_id bigint NOT NULL,
    revision integer NOT NULL,
    content_hash text NOT NULL,
    forward_sql text NOT NULL,
    inverse_sql text DEFAULT ''::text NOT NULL,
    evidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    preconditions jsonb DEFAULT '{}'::jsonb NOT NULL,
    policy_version bigint,
    title text DEFAULT ''::text NOT NULL,
    severity text DEFAULT ''::text NOT NULL,
    object_type text DEFAULT ''::text NOT NULL,
    action_risk text DEFAULT ''::text NOT NULL,
    recommendation text DEFAULT ''::text NOT NULL,
    source text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT recommendation_revision_forward_sql_check CHECK ((length(forward_sql) > 0)),
    CONSTRAINT recommendation_revision_revision_check CHECK ((revision > 0)),
    CONSTRAINT recommendation_revision_source_check CHECK ((source = ANY (ARRAY['analyzer'::text, 'migrated'::text])))
);


--
-- Name: recommendation_transition; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.recommendation_transition (
    id bigint NOT NULL,
    recommendation_id bigint NOT NULL,
    from_state text,
    to_state text NOT NULL,
    revision integer NOT NULL,
    actor text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    action_log_id bigint,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT recommendation_transition_revision_check CHECK ((revision > 0))
);


--
-- Name: recommendation_transition_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.recommendation_transition_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: recommendation_transition_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.recommendation_transition_id_seq OWNED BY sage.recommendation_transition.id;


--
-- Name: retention_run; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.retention_run (
    id bigint NOT NULL,
    database_id bigint,
    schema_name text NOT NULL,
    table_name text NOT NULL,
    retention_column text NOT NULL,
    cutoff_at timestamp with time zone NOT NULL,
    candidate_rows bigint DEFAULT 0 NOT NULL,
    deleted_rows bigint DEFAULT 0 NOT NULL,
    disposition text NOT NULL,
    relation_oid oid,
    column_attnum smallint,
    column_type text,
    contract_id bigint,
    contract_updated_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    action_id bigint,
    dry_run_id bigint,
    CONSTRAINT retention_run_disposition_check CHECK ((disposition = ANY (ARRAY['dry_run'::text, 'applied'::text, 'failed'::text])))
);


--
-- Name: retention_run_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.retention_run_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: retention_run_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.retention_run_id_seq OWNED BY sage.retention_run.id;


--
-- Name: rollout_instance; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.rollout_instance (
    run_evidence_id text NOT NULL,
    instance_id text NOT NULL,
    ordinal integer NOT NULL,
    status text NOT NULL,
    evidence_id text,
    action_log_id bigint,
    regression_pct double precision,
    detail text,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT rollout_instance_detail_check CHECK ((length(detail) <= 2000)),
    CONSTRAINT rollout_instance_ordinal_check CHECK ((ordinal > 0)),
    CONSTRAINT rollout_instance_status_check CHECK ((status = ANY (ARRAY['not_locally_verified'::text, 'applied'::text, 'rolled_back'::text, 'rollback_failed'::text, 'rollback_unavailable'::text, 'failed'::text])))
);


--
-- Name: rollout_run; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.rollout_run (
    id bigint NOT NULL,
    evidence_id text NOT NULL,
    source_instance text NOT NULL,
    prior_evidence_id text NOT NULL,
    policy jsonb NOT NULL,
    state text NOT NULL,
    aggregate_regression_pct numeric,
    applied_instances integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    halt_reason text,
    family text,
    action_class text,
    started_by text,
    CONSTRAINT rollout_run_state_check CHECK ((state = ANY (ARRAY['canary'::text, 'expanding'::text, 'halted'::text, 'complete'::text, 'failed'::text])))
);


--
-- Name: rollout_run_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.rollout_run_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: rollout_run_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.rollout_run_id_seq OWNED BY sage.rollout_run.id;


--
-- Name: runway_samples; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.runway_samples (
    id bigint NOT NULL,
    kind text NOT NULL,
    subject text NOT NULL,
    epoch text NOT NULL,
    sampled_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    value double precision NOT NULL,
    counter double precision,
    limit_value double precision,
    CONSTRAINT runway_samples_counter_check CHECK (((counter > '-Infinity'::double precision) AND (counter < 'Infinity'::double precision))),
    CONSTRAINT runway_samples_epoch_check CHECK (((length(epoch) >= 1) AND (length(epoch) <= 64))),
    CONSTRAINT runway_samples_kind_check CHECK (((length(kind) >= 1) AND (length(kind) <= 32))),
    CONSTRAINT runway_samples_limit_value_check CHECK (((limit_value > '-Infinity'::double precision) AND (limit_value < 'Infinity'::double precision))),
    CONSTRAINT runway_samples_subject_check CHECK (((length(subject) >= 1) AND (length(subject) <= 256))),
    CONSTRAINT runway_samples_value_check CHECK (((value > '-Infinity'::double precision) AND (value < 'Infinity'::double precision)))
);


--
-- Name: runway_samples_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.runway_samples_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: runway_samples_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.runway_samples_id_seq OWNED BY sage.runway_samples.id;


--
-- Name: schema_baseline; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.schema_baseline (
    id bigint NOT NULL,
    database_id bigint,
    object_identity text NOT NULL,
    object_type text NOT NULL,
    authorized_definition_hash text NOT NULL,
    observed_definition_hash text,
    last_authorized_decision_id bigint,
    last_authorized_action_id bigint,
    reconciliation_status text DEFAULT 'matched'::text NOT NULL,
    observed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT schema_baseline_reconciliation_status_check CHECK ((reconciliation_status = ANY (ARRAY['matched'::text, 'additive'::text, 'destructive'::text, 'ambiguous'::text, 'reconciling'::text])))
);


--
-- Name: schema_baseline_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.schema_baseline_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: schema_baseline_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.schema_baseline_id_seq OWNED BY sage.schema_baseline.id;


--
-- Name: schema_findings; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.schema_findings (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    rule_id text NOT NULL,
    schema_name text NOT NULL,
    table_name text NOT NULL,
    column_name text DEFAULT ''::text NOT NULL,
    index_name text DEFAULT ''::text NOT NULL,
    severity text NOT NULL,
    category text NOT NULL,
    description text NOT NULL,
    impact text DEFAULT ''::text NOT NULL,
    suggestion text,
    suggested_sql text,
    table_size bigint,
    impact_score real,
    first_seen timestamp with time zone DEFAULT now() NOT NULL,
    last_seen timestamp with time zone DEFAULT now() NOT NULL,
    resolved_at timestamp with time zone,
    suppressed boolean DEFAULT false NOT NULL,
    database_name text,
    query_count bigint,
    CONSTRAINT schema_findings_category_check CHECK ((category = ANY (ARRAY['safety'::text, 'performance'::text, 'correctness'::text, 'convention'::text, 'indexing'::text, 'data_integrity'::text, 'schema_design'::text, 'maintenance'::text]))),
    CONSTRAINT schema_findings_severity_check CHECK ((severity = ANY (ARRAY['info'::text, 'warning'::text, 'critical'::text])))
);


--
-- Name: sessions; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sessions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_id integer,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now()
);


--
-- Name: shadow_decision; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.shadow_decision (
    id bigint NOT NULL,
    database_id integer,
    database_name text DEFAULT ''::text NOT NULL,
    fingerprint text NOT NULL,
    family text NOT NULL,
    action_class text NOT NULL,
    finding_id bigint,
    recommendation_id bigint,
    title text DEFAULT ''::text NOT NULL,
    object_identifier text DEFAULT ''::text NOT NULL,
    sql text NOT NULL,
    rollback_sql text DEFAULT ''::text NOT NULL,
    shape text NOT NULL,
    prediction jsonb DEFAULT '{}'::jsonb NOT NULL,
    evidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    gate_verdict text NOT NULL,
    gate_reason text DEFAULT ''::text NOT NULL,
    trusted_verdict text NOT NULL,
    trusted_reason text DEFAULT ''::text NOT NULL,
    trusted_detail text DEFAULT ''::text NOT NULL,
    granted_level smallint NOT NULL,
    decision_id bigint,
    status text DEFAULT 'pending'::text NOT NULL,
    score text,
    score_source text,
    counted boolean DEFAULT false NOT NULL,
    score_reason text DEFAULT ''::text NOT NULL,
    score_detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    ref_action_log_id bigint,
    ref_queue_id bigint,
    applied_after timestamp with time zone,
    applied_detected_at timestamp with time zone,
    seen_count integer DEFAULT 1 NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    scored_at timestamp with time zone,
    CONSTRAINT shadow_decision_action_class_check CHECK ((action_class ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT shadow_decision_counted CHECK (((NOT counted) OR ((score = ANY (ARRAY['correct'::text, 'incorrect'::text, 'neutral'::text])) AND (score_source = ANY (ARRAY['external'::text, 'hypopg'::text]))))),
    CONSTRAINT shadow_decision_database_name_check CHECK ((length(database_name) <= 200)),
    CONSTRAINT shadow_decision_family_check CHECK ((family ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT shadow_decision_fingerprint_check CHECK (((length(fingerprint) >= 1) AND (length(fingerprint) <= 128))),
    CONSTRAINT shadow_decision_gate_verdict CHECK ((gate_verdict = ANY (ARRAY['observe_only'::text, 'queue_approval'::text, 'blocked'::text]))),
    CONSTRAINT shadow_decision_granted_level_check CHECK (((granted_level >= 0) AND (granted_level <= 4))),
    CONSTRAINT shadow_decision_score CHECK (((score IS NULL) OR (score = ANY (ARRAY['correct'::text, 'incorrect'::text, 'neutral'::text, 'unscored'::text])))),
    CONSTRAINT shadow_decision_scored CHECK ((((status = 'pending'::text) AND (score IS NULL) AND (score_source IS NULL) AND (NOT counted)) OR ((status = 'scored'::text) AND (score IS NOT NULL) AND (score_source IS NOT NULL)))),
    CONSTRAINT shadow_decision_source CHECK (((score_source IS NULL) OR (score_source = ANY (ARRAY['operator'::text, 'applied'::text, 'external'::text, 'hypopg'::text, 'none'::text])))),
    CONSTRAINT shadow_decision_sql_check CHECK ((length(sql) > 0)),
    CONSTRAINT shadow_decision_status CHECK ((status = ANY (ARRAY['pending'::text, 'scored'::text]))),
    CONSTRAINT shadow_decision_trusted_verdict CHECK ((trusted_verdict = ANY (ARRAY['execute'::text, 'queue_approval'::text, 'observe_only'::text, 'blocked'::text])))
);


--
-- Name: shadow_decision_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.shadow_decision_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: shadow_decision_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.shadow_decision_id_seq OWNED BY sage.shadow_decision.id;


--
-- Name: siem_cursor; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.siem_cursor (
    sink text NOT NULL,
    source text NOT NULL,
    chain text NOT NULL,
    seq bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: size_history; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.size_history (
    id bigint NOT NULL,
    collected_at timestamp with time zone DEFAULT now() NOT NULL,
    metric_type text NOT NULL,
    object_name text NOT NULL,
    size_bytes bigint NOT NULL,
    dead_tuple_pct numeric(5,2),
    database_name text,
    CONSTRAINT size_history_metric_type_check CHECK ((metric_type = ANY (ARRAY['database'::text, 'table'::text, 'wal_slot'::text])))
);


--
-- Name: size_history_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.size_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: size_history_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.size_history_id_seq OWNED BY sage.size_history.id;


--
-- Name: slot_consumer_registry; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.slot_consumer_registry (
    slot_name text NOT NULL,
    owner_tag text NOT NULL,
    consumer_identity text,
    registered boolean DEFAULT true NOT NULL,
    last_confirmed_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: snapshots; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.snapshots (
    id bigint NOT NULL,
    collected_at timestamp with time zone DEFAULT now() NOT NULL,
    category text NOT NULL,
    data jsonb NOT NULL,
    base_id bigint
)
PARTITION BY RANGE (collected_at);


--
-- Name: snapshots_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.snapshots_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: snapshots_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.snapshots_id_seq OWNED BY sage.snapshots.id;


--
-- Name: snapshots_default; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.snapshots_default (
    id bigint DEFAULT nextval('sage.snapshots_id_seq'::regclass) NOT NULL,
    collected_at timestamp with time zone DEFAULT now() NOT NULL,
    category text NOT NULL,
    data jsonb NOT NULL,
    base_id bigint
);


--
-- Name: snapshots_history; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.snapshots_history (
    id bigint DEFAULT nextval('sage.snapshots_id_seq'::regclass) NOT NULL,
    collected_at timestamp with time zone DEFAULT now() NOT NULL,
    category text NOT NULL,
    data jsonb NOT NULL,
    base_id bigint
);


--
-- Name: snapshots_p20261117; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.snapshots_p20261117 (
    id bigint DEFAULT nextval('sage.snapshots_id_seq'::regclass) NOT NULL,
    collected_at timestamp with time zone DEFAULT now() NOT NULL,
    category text NOT NULL,
    data jsonb NOT NULL,
    base_id bigint
);


--
-- Name: snapshots_p20261118; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.snapshots_p20261118 (
    id bigint DEFAULT nextval('sage.snapshots_id_seq'::regclass) NOT NULL,
    collected_at timestamp with time zone DEFAULT now() NOT NULL,
    category text NOT NULL,
    data jsonb NOT NULL,
    base_id bigint
);


--
-- Name: source_fix; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.source_fix (
    id bigint NOT NULL,
    finding_id bigint NOT NULL,
    packet_hash text NOT NULL,
    stage text NOT NULL,
    pr_url text,
    commit_sha text,
    reported_by text NOT NULL,
    deployed_at timestamp with time zone,
    prediction jsonb DEFAULT '{}'::jsonb NOT NULL,
    target_queryids bigint[] DEFAULT '{}'::bigint[] NOT NULL,
    verdict text,
    tolerance text,
    observed jsonb,
    evidence jsonb,
    reason text DEFAULT ''::text NOT NULL,
    decided_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT source_fix_check CHECK (((verdict IS NULL) = (decided_at IS NULL))),
    CONSTRAINT source_fix_check1 CHECK (((stage <> 'pr_opened'::text) OR (deployed_at IS NULL))),
    CONSTRAINT source_fix_commit_sha_check CHECK ((commit_sha ~ '^[0-9a-f]{7,64}$'::text)),
    CONSTRAINT source_fix_finding_id_check CHECK ((finding_id > 0)),
    CONSTRAINT source_fix_packet_hash_check CHECK (((length(packet_hash) >= 1) AND (length(packet_hash) <= 128))),
    CONSTRAINT source_fix_pr_url_check CHECK ((length(pr_url) <= 2048)),
    CONSTRAINT source_fix_reason_check CHECK ((length(reason) <= 2000)),
    CONSTRAINT source_fix_reported_by_check CHECK (((length(reported_by) >= 1) AND (length(reported_by) <= 200))),
    CONSTRAINT source_fix_stage_check CHECK ((stage = ANY (ARRAY['pr_opened'::text, 'deployed'::text, 'verified'::text]))),
    CONSTRAINT source_fix_verdict_check CHECK ((verdict = ANY (ARRAY['improved'::text, 'neutral'::text, 'regressed'::text, 'insufficient_evidence'::text, 'unverifiable'::text])))
);


--
-- Name: source_fix_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.source_fix_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: source_fix_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.source_fix_id_seq OWNED BY sage.source_fix.id;


--
-- Name: specialist_requests; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.specialist_requests (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    kind text NOT NULL,
    token_id text NOT NULL,
    identity_name text DEFAULT ''::text NOT NULL,
    actor text NOT NULL,
    transport text NOT NULL,
    database_name text NOT NULL,
    investigation_id text,
    created boolean DEFAULT false NOT NULL,
    match text DEFAULT ''::text NOT NULL,
    symptom jsonb,
    time_window jsonb,
    external_ref jsonb,
    remediation_id text DEFAULT ''::text NOT NULL,
    verdict text DEFAULT ''::text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    outbound text DEFAULT 'none'::text NOT NULL,
    outbound_attempts integer DEFAULT 0 NOT NULL,
    outbound_next_at timestamp with time zone DEFAULT now() NOT NULL,
    outbound_error text DEFAULT ''::text NOT NULL,
    terminal_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    query_scope jsonb,
    CONSTRAINT specialist_requests_actor_check CHECK (((length(actor) >= 1) AND (length(actor) <= 200))),
    CONSTRAINT specialist_requests_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 128))),
    CONSTRAINT specialist_requests_external_ref_check CHECK (((external_ref IS NULL) OR (jsonb_typeof(external_ref) = 'object'::text))),
    CONSTRAINT specialist_requests_identity_name_check CHECK ((length(identity_name) <= 200)),
    CONSTRAINT specialist_requests_investigation_id_check CHECK ((length(investigation_id) <= 64)),
    CONSTRAINT specialist_requests_kind_check CHECK ((kind = ANY (ARRAY['open'::text, 'attach'::text, 'remediation'::text]))),
    CONSTRAINT specialist_requests_match_check CHECK ((length(match) <= 32)),
    CONSTRAINT specialist_requests_outbound_attempts_check CHECK ((outbound_attempts >= 0)),
    CONSTRAINT specialist_requests_outbound_check CHECK ((outbound = ANY (ARRAY['none'::text, 'pending'::text, 'delivered'::text, 'failed'::text]))),
    CONSTRAINT specialist_requests_outbound_error_check CHECK ((length(outbound_error) <= 2000)),
    CONSTRAINT specialist_requests_query_scope_check CHECK (((query_scope IS NULL) OR ((jsonb_typeof(query_scope) = 'object'::text) AND (pg_column_size(query_scope) <= 1024)))),
    CONSTRAINT specialist_requests_reason_check CHECK ((length(reason) <= 2000)),
    CONSTRAINT specialist_requests_remediation_id_check CHECK ((length(remediation_id) <= 80)),
    CONSTRAINT specialist_requests_symptom_check CHECK (((symptom IS NULL) OR ((jsonb_typeof(symptom) = 'object'::text) AND (pg_column_size(symptom) <= 16384)))),
    CONSTRAINT specialist_requests_time_window_check CHECK (((time_window IS NULL) OR (jsonb_typeof(time_window) = 'object'::text))),
    CONSTRAINT specialist_requests_token_id_check CHECK (((length(token_id) >= 1) AND (length(token_id) <= 128))),
    CONSTRAINT specialist_requests_transport_check CHECK ((transport = ANY (ARRAY['http'::text, 'mcp'::text, 'pagerduty'::text, 'webhook'::text]))),
    CONSTRAINT specialist_requests_verdict_check CHECK ((length(verdict) <= 40))
);


--
-- Name: sre_action_proposals; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_action_proposals (
    id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    action_class text NOT NULL,
    family text DEFAULT ''::text NOT NULL,
    node_id text DEFAULT ''::text NOT NULL,
    state text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    evidence_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    target jsonb,
    target_sha256 bytea,
    baseline jsonb DEFAULT '{}'::jsonb NOT NULL,
    contract jsonb DEFAULT '{}'::jsonb NOT NULL,
    policy jsonb DEFAULT '{}'::jsonb NOT NULL,
    queue_id integer,
    finding_id bigint,
    action_log_id bigint,
    requested_by text DEFAULT ''::text NOT NULL,
    requested_at timestamp with time zone,
    decided_by integer,
    decided_at timestamp with time zone,
    executed_at timestamp with time zone,
    recovery jsonb DEFAULT '{}'::jsonb NOT NULL,
    recovery_state text DEFAULT ''::text NOT NULL,
    next_sample_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT sre_action_proposals_action_class_check CHECK ((action_class = 'cancel_backend'::text)),
    CONSTRAINT sre_action_proposals_baseline_check CHECK ((jsonb_typeof(baseline) = 'object'::text)),
    CONSTRAINT sre_action_proposals_contract_check CHECK ((jsonb_typeof(contract) = 'object'::text)),
    CONSTRAINT sre_action_proposals_detail_check CHECK ((length(detail) <= 2048)),
    CONSTRAINT sre_action_proposals_family_check CHECK ((length(family) <= 64)),
    CONSTRAINT sre_action_proposals_node_id_check CHECK ((length(node_id) <= 64)),
    CONSTRAINT sre_action_proposals_policy_check CHECK ((jsonb_typeof(policy) = 'object'::text)),
    CONSTRAINT sre_action_proposals_reason_check CHECK ((length(reason) <= 64)),
    CONSTRAINT sre_action_proposals_recovery_check CHECK (((jsonb_typeof(recovery) = 'object'::text) AND (octet_length((recovery)::text) <= 65536))),
    CONSTRAINT sre_action_proposals_recovery_state_check CHECK ((recovery_state = ANY (ARRAY[''::text, 'observing'::text, 'recovered'::text, 'not_recovered'::text, 'inconclusive'::text]))),
    CONSTRAINT sre_action_proposals_requested_by_check CHECK ((length(requested_by) <= 128)),
    CONSTRAINT sre_action_proposals_state_check CHECK ((state = ANY (ARRAY['ineligible'::text, 'proposed'::text, 'requested'::text, 'executing'::text, 'executed'::text, 'refused'::text, 'failed'::text, 'uncertain'::text, 'denied'::text, 'expired'::text]))),
    CONSTRAINT sre_action_proposals_target_check CHECK (((target IS NULL) OR (jsonb_typeof(target) = 'object'::text))),
    CONSTRAINT sre_action_proposals_target_sha256_check CHECK (((target_sha256 IS NULL) OR (octet_length(target_sha256) = 32))),
    CONSTRAINT sre_action_proposals_version_check CHECK ((version > 0))
);


--
-- Name: sre_autonomy_events; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_autonomy_events (
    id bigint NOT NULL,
    deployment_id uuid NOT NULL,
    family text NOT NULL,
    action_class text NOT NULL,
    event_type text NOT NULL,
    from_level smallint,
    to_level smallint,
    actor text NOT NULL,
    reason text NOT NULL,
    database_name text,
    proposal_id uuid,
    action_log_id bigint,
    evidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_autonomy_events_action_class_check CHECK ((action_class ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_autonomy_events_actor_check CHECK (((length(actor) >= 1) AND (length(actor) <= 200))),
    CONSTRAINT sre_autonomy_events_family_check CHECK ((family ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_autonomy_events_from_level_check CHECK (((from_level >= 0) AND (from_level <= 3))),
    CONSTRAINT sre_autonomy_events_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 2000))),
    CONSTRAINT sre_autonomy_events_to_level_check CHECK (((to_level >= 0) AND (to_level <= 3))),
    CONSTRAINT sre_autonomy_events_type_v2 CHECK ((event_type = ANY (ARRAY['promotion_proposed'::text, 'promotion_approved'::text, 'promotion_rejected'::text, 'promotion_expired'::text, 'downgraded'::text, 'auto_downgraded'::text, 'capped'::text, 'cap_cleared'::text, 'auto_executed'::text, 'carried_over'::text, 'deadline_override'::text, 'database_scoped'::text, 'grandfathered'::text, 'root_authority_granted'::text, 'root_authority_revoked'::text])))
);


--
-- Name: sre_autonomy_events_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.sre_autonomy_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: sre_autonomy_events_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.sre_autonomy_events_id_seq OWNED BY sage.sre_autonomy_events.id;


--
-- Name: sre_autonomy_outcomes; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_autonomy_outcomes (
    id bigint NOT NULL,
    deployment_id uuid NOT NULL,
    database_name text NOT NULL,
    action_log_id bigint,
    family text NOT NULL,
    action_class text NOT NULL,
    level smallint NOT NULL,
    result text NOT NULL,
    source text NOT NULL,
    actor text NOT NULL,
    detail text,
    recorded_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    verdict text,
    observed_at timestamp with time zone,
    queue_id bigint,
    CONSTRAINT sre_autonomy_outcomes_action_class_check CHECK ((action_class ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_autonomy_outcomes_actor_check CHECK (((length(actor) >= 1) AND (length(actor) <= 200))),
    CONSTRAINT sre_autonomy_outcomes_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 200))),
    CONSTRAINT sre_autonomy_outcomes_detail_check CHECK ((length(detail) <= 2000)),
    CONSTRAINT sre_autonomy_outcomes_family_check CHECK ((family ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_autonomy_outcomes_level_check CHECK (((level >= 0) AND (level <= 3))),
    CONSTRAINT sre_autonomy_outcomes_result_v2 CHECK ((result = ANY (ARRAY['verified_recovery'::text, 'not_recovered'::text, 'harmful'::text, 'safety_violation'::text, 'unverified'::text, 'rejected'::text]))),
    CONSTRAINT sre_autonomy_outcomes_source_v2 CHECK ((source = ANY (ARRAY['executor'::text, 'operator'::text, 'game_day'::text, 'rollout'::text, 'bench'::text, 'rollback'::text]))),
    CONSTRAINT sre_autonomy_outcomes_verdict CHECK (((verdict IS NULL) OR (verdict = ANY (ARRAY['improved'::text, 'neutral'::text, 'regressed'::text, 'insufficient_evidence'::text, 'unverifiable'::text, 'rolled_back'::text, 'rejected'::text]))))
);


--
-- Name: sre_autonomy_outcomes_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.sre_autonomy_outcomes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: sre_autonomy_outcomes_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.sre_autonomy_outcomes_id_seq OWNED BY sage.sre_autonomy_outcomes.id;


--
-- Name: sre_autonomy_proposals; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_autonomy_proposals (
    deployment_id uuid NOT NULL,
    id uuid NOT NULL,
    family text NOT NULL,
    action_class text NOT NULL,
    from_level smallint NOT NULL,
    to_level smallint NOT NULL,
    evidence jsonb NOT NULL,
    evidence_sha256 bytea NOT NULL,
    status text NOT NULL,
    proposed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    decided_by text,
    decided_at timestamp with time zone,
    decision_note text,
    database_name text DEFAULT ''::text NOT NULL,
    CONSTRAINT sre_autonomy_proposals_action_class_check CHECK ((action_class ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_autonomy_proposals_check CHECK ((to_level = (from_level + 1))),
    CONSTRAINT sre_autonomy_proposals_check1 CHECK ((expires_at > proposed_at)),
    CONSTRAINT sre_autonomy_proposals_check2 CHECK (((status = 'pending'::text) = (decided_at IS NULL))),
    CONSTRAINT sre_autonomy_proposals_database_name CHECK ((length(database_name) <= 200)),
    CONSTRAINT sre_autonomy_proposals_decision_note_check CHECK ((length(decision_note) <= 2000)),
    CONSTRAINT sre_autonomy_proposals_evidence_sha256_check CHECK ((octet_length(evidence_sha256) = 32)),
    CONSTRAINT sre_autonomy_proposals_family_check CHECK ((family ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_autonomy_proposals_from_level_check CHECK (((from_level >= 0) AND (from_level <= 2))),
    CONSTRAINT sre_autonomy_proposals_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'rejected'::text, 'expired'::text, 'superseded'::text]))),
    CONSTRAINT sre_autonomy_proposals_to_level_check CHECK (((to_level >= 1) AND (to_level <= 3)))
);


--
-- Name: sre_budget_reservations; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_budget_reservations (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    id uuid NOT NULL,
    investigation_id uuid,
    utc_day date NOT NULL,
    caller_kind text NOT NULL,
    request_key text NOT NULL,
    state text NOT NULL,
    input_reserved bigint NOT NULL,
    output_reserved bigint NOT NULL,
    input_used bigint,
    output_used bigint,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    settled_at timestamp with time zone,
    reasoning_reserved bigint DEFAULT 0 NOT NULL,
    reasoning_used bigint,
    CONSTRAINT sre_budget_reservations_check CHECK (((input_used IS NULL) = (output_used IS NULL))),
    CONSTRAINT sre_budget_reservations_check1 CHECK (((state = 'settled'::text) = (input_used IS NOT NULL))),
    CONSTRAINT sre_budget_reservations_input_reserved_check CHECK ((input_reserved >= 0)),
    CONSTRAINT sre_budget_reservations_input_used_check CHECK ((input_used >= 0)),
    CONSTRAINT sre_budget_reservations_output_reserved_check CHECK ((output_reserved >= 0)),
    CONSTRAINT sre_budget_reservations_output_used_check CHECK ((output_used >= 0)),
    CONSTRAINT sre_budget_reservations_reasoning_reserved_check CHECK ((reasoning_reserved >= 0)),
    CONSTRAINT sre_budget_reservations_reasoning_used_check CHECK ((reasoning_used >= 0)),
    CONSTRAINT sre_budget_reservations_request_key_check CHECK (((length(request_key) >= 1) AND (length(request_key) <= 160))),
    CONSTRAINT sre_budget_reservations_state_check CHECK ((state = ANY (ARRAY['reserved'::text, 'inflight'::text, 'settled'::text, 'unknown'::text, 'cancelled'::text]))),
    CONSTRAINT sre_budget_reservations_version_check CHECK ((version > 0))
)
WITH (autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: sre_change_events; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_change_events (
    deployment_id uuid NOT NULL,
    id uuid NOT NULL,
    database_id uuid,
    source text NOT NULL,
    event_id text NOT NULL,
    kind text NOT NULL,
    service text,
    summary text NOT NULL,
    link text,
    objects jsonb DEFAULT '[]'::jsonb NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    received_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    signature_status text NOT NULL,
    change_hash bytea NOT NULL,
    CONSTRAINT sre_change_events_change_hash_check CHECK ((octet_length(change_hash) = 32)),
    CONSTRAINT sre_change_events_event_id_check CHECK (((length(event_id) >= 1) AND (length(event_id) <= 128))),
    CONSTRAINT sre_change_events_kind_check CHECK ((kind = ANY (ARRAY['deploy'::text, 'migration'::text, 'feature_flag'::text, 'config'::text, 'ddl'::text, 'sage_action'::text, 'stats_reset'::text, 'restart'::text, 'failover'::text, 'extension'::text, 'other'::text]))),
    CONSTRAINT sre_change_events_link_check CHECK ((length(link) <= 512)),
    CONSTRAINT sre_change_events_objects_check CHECK ((jsonb_typeof(objects) = 'array'::text)),
    CONSTRAINT sre_change_events_service_check CHECK ((length(service) <= 128)),
    CONSTRAINT sre_change_events_signature_status_check CHECK ((signature_status = ANY (ARRAY['verified'::text, 'internal'::text]))),
    CONSTRAINT sre_change_events_source_check CHECK (((length(source) >= 1) AND (length(source) <= 64))),
    CONSTRAINT sre_change_events_summary_check CHECK (((length(summary) >= 1) AND (length(summary) <= 300)))
);


--
-- Name: sre_change_feed_state; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_change_feed_state (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    source text NOT NULL,
    state jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_change_feed_state_source_check CHECK (((length(source) >= 1) AND (length(source) <= 64))),
    CONSTRAINT sre_change_feed_state_state_check CHECK ((jsonb_typeof(state) = 'object'::text))
)
WITH (fillfactor='50', autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: sre_database_bindings; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_database_bindings (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    runtime_key text NOT NULL,
    legacy_database_id integer,
    identity_strength text NOT NULL,
    cluster_epoch text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_database_bindings_cluster_epoch_check CHECK (((length(cluster_epoch) >= 1) AND (length(cluster_epoch) <= 128))),
    CONSTRAINT sre_database_bindings_identity_strength_check CHECK ((identity_strength = ANY (ARRAY['configured'::text, 'provider'::text, 'cluster'::text]))),
    CONSTRAINT sre_database_bindings_runtime_key_check CHECK (((length(runtime_key) >= 1) AND (length(runtime_key) <= 128)))
);


--
-- Name: sre_deployments; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_deployments (
    singleton boolean DEFAULT true NOT NULL,
    deployment_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_deployments_singleton_check CHECK (singleton)
);


--
-- Name: sre_eval_runs; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_eval_runs (
    deployment_id uuid NOT NULL,
    id uuid NOT NULL,
    source text NOT NULL,
    schema_version text NOT NULL,
    generated_at timestamp with time zone NOT NULL,
    ingested_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    ingested_by text NOT NULL,
    database_name text,
    report_sha256 bytea NOT NULL,
    gated_arms jsonb DEFAULT '[]'::jsonb NOT NULL,
    cells jsonb NOT NULL,
    origin text DEFAULT 'operator'::text NOT NULL,
    pg_sage_version text DEFAULT ''::text NOT NULL,
    pg_sage_commit text DEFAULT ''::text NOT NULL,
    signature jsonb,
    model_lift jsonb,
    CONSTRAINT sre_eval_runs_check CHECK (((source = 'game_day'::text) = (database_name IS NOT NULL))),
    CONSTRAINT sre_eval_runs_ingested_by_check CHECK (((length(ingested_by) >= 1) AND (length(ingested_by) <= 200))),
    CONSTRAINT sre_eval_runs_model_lift_object CHECK (((model_lift IS NULL) OR (jsonb_typeof(model_lift) = 'object'::text))),
    CONSTRAINT sre_eval_runs_origin CHECK (((origin = ANY (ARRAY['signed_release'::text, 'local_run'::text, 'operator'::text, 'game_day'::text])) AND ((origin = 'signed_release'::text) = (signature IS NOT NULL)) AND (length(pg_sage_version) <= 64) AND (length(pg_sage_commit) <= 64))),
    CONSTRAINT sre_eval_runs_report_sha256_check CHECK ((octet_length(report_sha256) = 32)),
    CONSTRAINT sre_eval_runs_source_check CHECK ((source = ANY (ARRAY['bench'::text, 'game_day'::text])))
);


--
-- Name: sre_events; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_events (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    sequence integer NOT NULL,
    event_type text NOT NULL,
    actor text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    previous_hash bytea,
    hash bytea NOT NULL,
    CONSTRAINT sre_events_actor_check CHECK (((length(actor) >= 1) AND (length(actor) <= 128))),
    CONSTRAINT sre_events_check CHECK (((sequence = 1) = (previous_hash IS NULL))),
    CONSTRAINT sre_events_event_type_m3 CHECK ((event_type = ANY (ARRAY['action_decided'::text, 'action_executed'::text, 'action_failed'::text, 'action_proposed'::text, 'action_recheck'::text, 'action_refused'::text, 'action_requested'::text, 'claimed'::text, 'concluded'::text, 'created'::text, 'evidence_purged'::text, 'model_disagreed'::text, 'model_rejected'::text, 'model_reviewed'::text, 'pinned'::text, 'recovery_sample'::text, 'recovery_verdict'::text, 'step'::text, 'transition'::text, 'unpinned'::text]))),
    CONSTRAINT sre_events_hash_check CHECK ((octet_length(hash) = 32)),
    CONSTRAINT sre_events_payload_check CHECK ((jsonb_typeof(payload) = 'object'::text)),
    CONSTRAINT sre_events_payload_check1 CHECK ((octet_length((payload)::text) <= 16384)),
    CONSTRAINT sre_events_previous_hash_check CHECK (((previous_hash IS NULL) OR (octet_length(previous_hash) = 32))),
    CONSTRAINT sre_events_sequence_check CHECK ((sequence > 0))
);


--
-- Name: sre_evidence; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_evidence (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id uuid NOT NULL,
    step_id uuid NOT NULL,
    source_kind text NOT NULL,
    probe_id text NOT NULL,
    probe_version text NOT NULL,
    observed_at timestamp with time zone,
    collected_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    capability_state text NOT NULL,
    reason_code text,
    payload_version integer NOT NULL,
    classification text NOT NULL,
    payload jsonb NOT NULL,
    sha256 bytea NOT NULL,
    CONSTRAINT sre_evidence_capability_state_check CHECK ((capability_state = ANY (ARRAY['available'::text, 'unsupported'::text, 'permission_denied'::text, 'unreachable'::text, 'unknown'::text]))),
    CONSTRAINT sre_evidence_check CHECK (((capability_state = 'available'::text) OR (reason_code IS NOT NULL))),
    CONSTRAINT sre_evidence_classification_check CHECK ((classification = ANY (ARRAY['redacted'::text, 'restricted'::text]))),
    CONSTRAINT sre_evidence_payload_check CHECK ((jsonb_typeof(payload) = 'object'::text)),
    CONSTRAINT sre_evidence_payload_check1 CHECK ((octet_length((payload)::text) <= 262144)),
    CONSTRAINT sre_evidence_payload_version_check CHECK ((payload_version > 0)),
    CONSTRAINT sre_evidence_probe_id_check CHECK (((length(probe_id) >= 1) AND (length(probe_id) <= 64))),
    CONSTRAINT sre_evidence_sha256_check CHECK ((octet_length(sha256) = 32))
);


--
-- Name: sre_family_autonomy; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_family_autonomy (
    deployment_id uuid NOT NULL,
    family text NOT NULL,
    action_class text NOT NULL,
    level smallint NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    evidence jsonb DEFAULT '{}'::jsonb NOT NULL,
    changed_by text NOT NULL,
    change_reason text NOT NULL,
    changed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    provenance text DEFAULT 'ledger'::text NOT NULL,
    carried_ref text,
    database_name text DEFAULT ''::text NOT NULL,
    CONSTRAINT sre_family_autonomy_action_class_check CHECK ((action_class ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_family_autonomy_change_reason_check CHECK (((length(change_reason) >= 1) AND (length(change_reason) <= 1000))),
    CONSTRAINT sre_family_autonomy_changed_by_check CHECK (((length(changed_by) >= 1) AND (length(changed_by) <= 200))),
    CONSTRAINT sre_family_autonomy_database_name CHECK ((length(database_name) <= 200)),
    CONSTRAINT sre_family_autonomy_family_check CHECK ((family ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_family_autonomy_level_check CHECK (((level >= 0) AND (level <= 3))),
    CONSTRAINT sre_family_autonomy_provenance CHECK ((((provenance = 'ledger'::text) AND (carried_ref IS NULL)) OR ((provenance = ANY (ARRAY['carried_over'::text, 'grandfathered'::text])) AND (carried_ref IS NOT NULL) AND ((length(carried_ref) >= 1) AND (length(carried_ref) <= 500))))),
    CONSTRAINT sre_family_autonomy_version_check CHECK ((version > 0))
)
WITH (autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: sre_game_days; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_game_days (
    deployment_id uuid NOT NULL,
    id uuid NOT NULL,
    database_name text NOT NULL,
    provider text NOT NULL,
    clone_id text,
    families text[] NOT NULL,
    status text NOT NULL,
    started_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    finished_at timestamp with time zone,
    error text,
    eval_run_id uuid,
    CONSTRAINT sre_game_days_check CHECK (((status = 'running'::text) = (finished_at IS NULL))),
    CONSTRAINT sre_game_days_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 200))),
    CONSTRAINT sre_game_days_error_check CHECK ((length(error) <= 2000)),
    CONSTRAINT sre_game_days_status_check CHECK ((status = ANY (ARRAY['running'::text, 'completed'::text, 'failed'::text, 'destroy_failed'::text])))
);


--
-- Name: sre_hypotheses; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_hypotheses (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id uuid NOT NULL,
    revision integer NOT NULL,
    ordinal integer NOT NULL,
    graph_version text NOT NULL,
    family text NOT NULL,
    node_id text NOT NULL,
    label text NOT NULL,
    mechanism text NOT NULL,
    subject text DEFAULT ''::text NOT NULL,
    status text NOT NULL,
    confidence double precision NOT NULL,
    supporting_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    contradicting_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    support jsonb NOT NULL,
    contradict jsonb NOT NULL,
    refutation_probe text NOT NULL,
    operator_step text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_hypotheses_check CHECK (((octet_length((support)::text) + octet_length((contradict)::text)) <= 65536)),
    CONSTRAINT sre_hypotheses_check1 CHECK (((status <> 'ruled_out'::text) OR (cardinality(contradicting_ids) > 0))),
    CONSTRAINT sre_hypotheses_confidence_check CHECK (((confidence >= (0)::double precision) AND (confidence <= (1)::double precision))),
    CONSTRAINT sre_hypotheses_contradict_check CHECK ((jsonb_typeof(contradict) = 'array'::text)),
    CONSTRAINT sre_hypotheses_family_check CHECK (((length(family) >= 1) AND (length(family) <= 64))),
    CONSTRAINT sre_hypotheses_graph_version_check CHECK (((length(graph_version) >= 1) AND (length(graph_version) <= 32))),
    CONSTRAINT sre_hypotheses_label_check CHECK (((length(label) >= 1) AND (length(label) <= 256))),
    CONSTRAINT sre_hypotheses_mechanism_check CHECK ((length(mechanism) <= 1024)),
    CONSTRAINT sre_hypotheses_node_id_check CHECK (((length(node_id) >= 1) AND (length(node_id) <= 64))),
    CONSTRAINT sre_hypotheses_operator_step_check CHECK ((length(operator_step) <= 512)),
    CONSTRAINT sre_hypotheses_ordinal_check CHECK ((ordinal > 0)),
    CONSTRAINT sre_hypotheses_refutation_probe_check CHECK (((length(refutation_probe) >= 1) AND (length(refutation_probe) <= 64))),
    CONSTRAINT sre_hypotheses_revision_check CHECK ((revision > 0)),
    CONSTRAINT sre_hypotheses_status_check CHECK ((status = ANY (ARRAY['root_cause'::text, 'contributing'::text, 'unproven'::text, 'ruled_out'::text]))),
    CONSTRAINT sre_hypotheses_subject_check CHECK ((length(subject) <= 256)),
    CONSTRAINT sre_hypotheses_support_check CHECK ((jsonb_typeof(support) = 'array'::text))
);


--
-- Name: sre_investigation_outcomes; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_investigation_outcomes (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id uuid NOT NULL,
    verdict text NOT NULL,
    actual_node text,
    actor text NOT NULL,
    recorded_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_investigation_outcomes_actor_check CHECK (((length(actor) >= 1) AND (length(actor) <= 128))),
    CONSTRAINT sre_investigation_outcomes_actual_node_check CHECK (((length(actual_node) >= 1) AND (length(actual_node) <= 64))),
    CONSTRAINT sre_investigation_outcomes_verdict_check CHECK ((verdict = ANY (ARRAY['confirmed'::text, 'refuted'::text])))
);


--
-- Name: sre_investigations; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_investigations (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    id uuid NOT NULL,
    source_case_id text NOT NULL,
    trigger_kind text NOT NULL,
    trigger_fingerprint bytea NOT NULL,
    idempotency_key text,
    state text NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    fence_token bigint DEFAULT 0 NOT NULL,
    lease_owner uuid,
    lease_started_at timestamp with time zone,
    lease_until timestamp with time zone,
    segment_deadline timestamp with time zone,
    active_ms bigint DEFAULT 0 NOT NULL,
    probe_count integer DEFAULT 0 NOT NULL,
    model_turns integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    failure_code text,
    source_incident_id text,
    subject text DEFAULT ''::text NOT NULL,
    pinned boolean DEFAULT false NOT NULL,
    summary jsonb DEFAULT '{}'::jsonb NOT NULL,
    concluded_at timestamp with time zone,
    evidence_purged_at timestamp with time zone,
    CONSTRAINT sre_investigations_active_ms_check CHECK (((active_ms >= 0) AND (active_ms <= 120000))),
    CONSTRAINT sre_investigations_check CHECK (((lease_owner IS NULL) = (lease_until IS NULL))),
    CONSTRAINT sre_investigations_check1 CHECK (((lease_owner IS NULL) = (lease_started_at IS NULL))),
    CONSTRAINT sre_investigations_check2 CHECK ((expires_at > created_at)),
    CONSTRAINT sre_investigations_fence_token_check CHECK ((fence_token >= 0)),
    CONSTRAINT sre_investigations_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 160))),
    CONSTRAINT sre_investigations_m2_bounds CHECK (((length(COALESCE(source_incident_id, ''::text)) <= 256) AND (length(subject) <= 256) AND (jsonb_typeof(summary) = 'object'::text) AND (octet_length((summary)::text) <= 65536))),
    CONSTRAINT sre_investigations_model_turns_check CHECK (((model_turns >= 0) AND (model_turns <= 2))),
    CONSTRAINT sre_investigations_probe_count_check CHECK (((probe_count >= 0) AND (probe_count <= 12))),
    CONSTRAINT sre_investigations_source_case_id_check CHECK (((length(source_case_id) >= 1) AND (length(source_case_id) <= 256))),
    CONSTRAINT sre_investigations_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'collecting'::text, 'evaluating'::text, 'needs_evidence'::text, 'concluded'::text, 'inconclusive'::text, 'paused'::text, 'cancelled'::text, 'expired'::text, 'failed'::text]))),
    CONSTRAINT sre_investigations_trigger_fingerprint_check CHECK ((octet_length(trigger_fingerprint) = 32)),
    CONSTRAINT sre_investigations_version_check CHECK ((version > 0))
)
WITH (fillfactor='70', autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: sre_maintenance_proposals; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_maintenance_proposals (
    id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    state text NOT NULL,
    proposal jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_maintenance_proposals_proposal_check CHECK (((jsonb_typeof(proposal) = 'object'::text) AND (octet_length((proposal)::text) <= 65536))),
    CONSTRAINT sre_maintenance_proposals_state_check CHECK ((state = ANY (ARRAY['proposed'::text, 'requested'::text, 'executing'::text, 'executed'::text, 'uncertain'::text, 'failed'::text, 'refused'::text, 'rolling_back'::text, 'rolled_back'::text, 'rollback_failed'::text])))
);


--
-- Name: sre_packet_reviews; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_packet_reviews (
    deployment_id uuid NOT NULL,
    database_name text NOT NULL,
    investigation_id uuid NOT NULL,
    family text NOT NULL,
    verdict text NOT NULL,
    reviewer text NOT NULL,
    note text,
    reviewed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    counts_as_evidence boolean DEFAULT true NOT NULL,
    CONSTRAINT sre_packet_reviews_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 200))),
    CONSTRAINT sre_packet_reviews_family_check CHECK ((family ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT sre_packet_reviews_note_check CHECK ((length(note) <= 2000)),
    CONSTRAINT sre_packet_reviews_reviewer_check CHECK (((length(reviewer) >= 1) AND (length(reviewer) <= 200))),
    CONSTRAINT sre_packet_reviews_verdict_check CHECK ((verdict = ANY (ARRAY['accepted'::text, 'rejected'::text])))
);


--
-- Name: sre_runbook_runs; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_runbook_runs (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    runbook_id uuid NOT NULL,
    version integer NOT NULL,
    content_hash bytea NOT NULL,
    outcome text NOT NULL,
    path jsonb NOT NULL,
    proposal jsonb,
    reason text DEFAULT ''::text NOT NULL,
    probes integer NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_runbook_runs_content_hash_check CHECK ((octet_length(content_hash) = 32)),
    CONSTRAINT sre_runbook_runs_outcome_check CHECK ((outcome = ANY (ARRAY['completed'::text, 'abstained'::text, 'probe_budget_exhausted'::text, 'no_time'::text]))),
    CONSTRAINT sre_runbook_runs_path_check CHECK (((jsonb_typeof(path) = 'array'::text) AND (octet_length((path)::text) <= 4096))),
    CONSTRAINT sre_runbook_runs_probes_check CHECK (((probes >= 0) AND (probes <= 12))),
    CONSTRAINT sre_runbook_runs_proposal_check CHECK (((proposal IS NULL) OR ((jsonb_typeof(proposal) = 'object'::text) AND (octet_length((proposal)::text) <= 4096)))),
    CONSTRAINT sre_runbook_runs_reason_check CHECK ((length(reason) <= 500)),
    CONSTRAINT sre_runbook_runs_version_check CHECK ((version > 0))
);


--
-- Name: sre_runbook_versions; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_runbook_versions (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    runbook_id uuid NOT NULL,
    version integer NOT NULL,
    name text NOT NULL,
    definition jsonb NOT NULL,
    content_hash bytea NOT NULL,
    source text NOT NULL,
    source_text text,
    compiled_by text,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    signed_by text,
    signer_role text,
    signed_hash bytea,
    signed_at timestamp with time zone,
    CONSTRAINT sre_runbook_versions_check CHECK (((signed_at IS NULL) = (signed_by IS NULL))),
    CONSTRAINT sre_runbook_versions_check1 CHECK (((signed_at IS NULL) = (signed_hash IS NULL))),
    CONSTRAINT sre_runbook_versions_check2 CHECK (((signed_at IS NULL) = (signer_role IS NULL))),
    CONSTRAINT sre_runbook_versions_check3 CHECK (((signed_hash IS NULL) OR (signed_hash = content_hash))),
    CONSTRAINT sre_runbook_versions_compiled_by_check CHECK ((length(compiled_by) <= 128)),
    CONSTRAINT sre_runbook_versions_content_hash_check CHECK ((octet_length(content_hash) = 32)),
    CONSTRAINT sre_runbook_versions_created_by_check CHECK (((length(created_by) >= 1) AND (length(created_by) <= 128))),
    CONSTRAINT sre_runbook_versions_definition_check CHECK (((jsonb_typeof(definition) = 'object'::text) AND (octet_length((definition)::text) <= 32768))),
    CONSTRAINT sre_runbook_versions_name_check CHECK (((length(name) >= 1) AND (length(name) <= 120))),
    CONSTRAINT sre_runbook_versions_signed_by_check CHECK (((length(signed_by) >= 1) AND (length(signed_by) <= 128))),
    CONSTRAINT sre_runbook_versions_signer_role_check CHECK ((signer_role = 'admin'::text)),
    CONSTRAINT sre_runbook_versions_source_check CHECK ((source = ANY (ARRAY['manual'::text, 'compiled'::text]))),
    CONSTRAINT sre_runbook_versions_source_text_check CHECK ((length(source_text) <= 16000)),
    CONSTRAINT sre_runbook_versions_version_check CHECK ((version > 0))
);


--
-- Name: sre_runbooks; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_runbooks (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    id uuid NOT NULL,
    latest_version integer DEFAULT 1 NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    retired_by text,
    retired_at timestamp with time zone,
    CONSTRAINT sre_runbooks_check CHECK (((retired_at IS NULL) = (retired_by IS NULL))),
    CONSTRAINT sre_runbooks_created_by_check CHECK (((length(created_by) >= 1) AND (length(created_by) <= 128))),
    CONSTRAINT sre_runbooks_latest_version_check CHECK ((latest_version > 0)),
    CONSTRAINT sre_runbooks_retired_by_check CHECK (((length(retired_by) >= 1) AND (length(retired_by) <= 128)))
);


--
-- Name: sre_service_slos; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_service_slos (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    source text NOT NULL,
    target double precision NOT NULL,
    definition_hash text NOT NULL,
    state text NOT NULL,
    fast_burning boolean DEFAULT false NOT NULL,
    status jsonb NOT NULL,
    evaluated_at timestamp with time zone NOT NULL,
    state_since timestamp with time zone NOT NULL,
    burn_started_at timestamp with time zone,
    version bigint DEFAULT 1 NOT NULL,
    CONSTRAINT sre_service_slos_definition_hash_check CHECK (((length(definition_hash) >= 1) AND (length(definition_hash) <= 64))),
    CONSTRAINT sre_service_slos_kind_check CHECK ((kind = ANY (ARRAY['app'::text, 'proxy'::text]))),
    CONSTRAINT sre_service_slos_name_check CHECK ((name ~ '^[a-z0-9][a-z0-9_.-]{0,63}$'::text)),
    CONSTRAINT sre_service_slos_source_check CHECK ((source = ANY (ARRAY['prometheus'::text, 'push'::text, 'proxy'::text]))),
    CONSTRAINT sre_service_slos_state_check CHECK ((state = ANY (ARRAY['ok'::text, 'ticket'::text, 'page'::text, 'unknown'::text]))),
    CONSTRAINT sre_service_slos_status_check CHECK (((jsonb_typeof(status) = 'object'::text) AND (octet_length((status)::text) <= 65536))),
    CONSTRAINT sre_service_slos_target_check CHECK (((target > (0)::double precision) AND (target < (1)::double precision))),
    CONSTRAINT sre_service_slos_version_check CHECK ((version > 0))
)
WITH (fillfactor='50', autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: sre_sli_samples; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_sli_samples (
    deployment_id uuid NOT NULL,
    slo_name text NOT NULL,
    series text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    bad double precision NOT NULL,
    eligible double precision NOT NULL,
    value double precision,
    received_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    cum_bad double precision,
    cum_eligible double precision,
    cum_samples bigint,
    cum_resets bigint,
    chain_start timestamp with time zone,
    CONSTRAINT sre_sli_samples_bad_check CHECK ((bad >= (0)::double precision)),
    CONSTRAINT sre_sli_samples_check CHECK ((bad <= eligible)),
    CONSTRAINT sre_sli_samples_eligible_check CHECK ((eligible >= (0)::double precision)),
    CONSTRAINT sre_sli_samples_series_check CHECK (((length(series) >= 1) AND (length(series) <= 128))),
    CONSTRAINT sre_sli_samples_slo_name_check CHECK (((length(slo_name) >= 1) AND (length(slo_name) <= 64)))
);


--
-- Name: sre_slo_transitions; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_slo_transitions (
    id bigint NOT NULL,
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    name text NOT NULL,
    from_state text,
    to_state text NOT NULL,
    at timestamp with time zone NOT NULL,
    CONSTRAINT sre_slo_transitions_from_state_check CHECK ((from_state = ANY (ARRAY['ok'::text, 'ticket'::text, 'page'::text, 'unknown'::text]))),
    CONSTRAINT sre_slo_transitions_to_state_check CHECK ((to_state = ANY (ARRAY['ok'::text, 'ticket'::text, 'page'::text, 'unknown'::text])))
);


--
-- Name: sre_slo_transitions_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

ALTER TABLE sage.sre_slo_transitions ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME sage.sre_slo_transitions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: sre_steps; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_steps (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    id uuid NOT NULL,
    sequence integer NOT NULL,
    idempotency_key text NOT NULL,
    fence_token bigint NOT NULL,
    probe_count integer NOT NULL,
    next_state text NOT NULL,
    error_code text,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT sre_steps_fence_token_check CHECK ((fence_token > 0)),
    CONSTRAINT sre_steps_idempotency_key_check CHECK (((length(idempotency_key) >= 1) AND (length(idempotency_key) <= 160))),
    CONSTRAINT sre_steps_probe_count_check CHECK ((probe_count >= 0)),
    CONSTRAINT sre_steps_sequence_check CHECK ((sequence > 0))
);


--
-- Name: sre_tombstones; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.sre_tombstones (
    deployment_id uuid NOT NULL,
    database_id uuid NOT NULL,
    investigation_id uuid NOT NULL,
    kind text NOT NULL,
    deleted_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    reason text NOT NULL,
    row_count integer NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT sre_tombstones_detail_check CHECK ((jsonb_typeof(detail) = 'object'::text)),
    CONSTRAINT sre_tombstones_kind_check CHECK ((kind = ANY (ARRAY['evidence'::text, 'investigation'::text]))),
    CONSTRAINT sre_tombstones_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 128))),
    CONSTRAINT sre_tombstones_row_count_check CHECK ((row_count >= 0))
);


--
-- Name: table_contract; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.table_contract (
    id bigint NOT NULL,
    database_id bigint,
    schema_name text NOT NULL,
    table_name text NOT NULL,
    append_only boolean DEFAULT false NOT NULL,
    retention_interval interval,
    retention_column text,
    expected_pk text,
    exemptions jsonb DEFAULT '[]'::jsonb NOT NULL,
    declared_by text NOT NULL,
    evidence_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: table_contract_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.table_contract_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: table_contract_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.table_contract_id_seq OWNED BY sage.table_contract.id;


--
-- Name: toil_model; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.toil_model (
    action_type text NOT NULL,
    model_version integer NOT NULL,
    base_minutes numeric NOT NULL,
    notes text,
    provenance text DEFAULT 'pg_sage_seed'::text NOT NULL,
    effective_from timestamp with time zone DEFAULT now() NOT NULL,
    effective_to timestamp with time zone,
    CONSTRAINT toil_model_base_minutes_check CHECK ((base_minutes >= (0)::numeric))
);


--
-- Name: trust_ledger_state; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.trust_ledger_state (
    deployment_id uuid NOT NULL,
    database_name text NOT NULL,
    grandfathered_at timestamp with time zone,
    bound jsonb,
    seeded jsonb,
    verdict_cursor timestamp with time zone,
    rollback_cursor timestamp with time zone,
    rejection_cursor timestamp with time zone,
    updated_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    shadow_cursor timestamp with time zone,
    CONSTRAINT trust_ledger_state_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 200)))
)
WITH (autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: trust_shadow_evidence; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.trust_shadow_evidence (
    id bigint NOT NULL,
    deployment_id uuid NOT NULL,
    database_name text NOT NULL,
    shadow_id bigint NOT NULL,
    fingerprint text NOT NULL,
    family text NOT NULL,
    action_class text NOT NULL,
    score text NOT NULL,
    source text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    recorded_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT trust_shadow_evidence_action_class_check CHECK ((action_class ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT trust_shadow_evidence_database_name_check CHECK (((length(database_name) >= 1) AND (length(database_name) <= 200))),
    CONSTRAINT trust_shadow_evidence_family_check CHECK ((family ~ '^[a-z][a-z0-9_]{0,63}$'::text)),
    CONSTRAINT trust_shadow_evidence_fingerprint_check CHECK (((length(fingerprint) >= 1) AND (length(fingerprint) <= 128))),
    CONSTRAINT trust_shadow_evidence_score_check CHECK ((score = ANY (ARRAY['correct'::text, 'incorrect'::text, 'neutral'::text]))),
    CONSTRAINT trust_shadow_evidence_source_check CHECK ((source = ANY (ARRAY['external'::text, 'hypopg'::text])))
);


--
-- Name: trust_shadow_evidence_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.trust_shadow_evidence_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: trust_shadow_evidence_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.trust_shadow_evidence_id_seq OWNED BY sage.trust_shadow_evidence.id;


--
-- Name: tuning_budget_day; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.tuning_budget_day (
    database_name text DEFAULT current_database() NOT NULL,
    utc_day date NOT NULL,
    tokens_used bigint DEFAULT 0 NOT NULL,
    requests_used bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT tuning_budget_day_requests_used_check CHECK ((requests_used >= 0)),
    CONSTRAINT tuning_budget_day_tokens_used_check CHECK ((tokens_used >= 0))
);


--
-- Name: user_oidc_link_grants; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.user_oidc_link_grants (
    id bigint NOT NULL,
    user_id integer NOT NULL,
    token_hash text NOT NULL,
    created_by integer DEFAULT 0 NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    used_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: user_oidc_link_grants_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.user_oidc_link_grants_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: user_oidc_link_grants_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.user_oidc_link_grants_id_seq OWNED BY sage.user_oidc_link_grants.id;


--
-- Name: users; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.users (
    id integer NOT NULL,
    email text NOT NULL,
    password text,
    role text DEFAULT 'viewer'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now(),
    last_login timestamp with time zone,
    oauth_provider text DEFAULT ''::text,
    oauth_issuer text,
    oauth_subject text
);


--
-- Name: users_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.users_id_seq
    AS integer
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: users_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.users_id_seq OWNED BY sage.users.id;


--
-- Name: value_rollup; Type: VIEW; Schema: sage; Owner: -
--

CREATE VIEW sage.value_rollup AS
 SELECT date_trunc('day'::text, al.executed_at) AS day,
    al.database_id,
    al.action_type,
    count(*) FILTER (WHERE ((al.outcome = 'success'::text) AND (al.toil_minutes_saved IS NOT NULL))) AS actions_verified,
    COALESCE(sum(al.toil_minutes_saved) FILTER (WHERE (al.outcome = 'success'::text)), (0)::numeric) AS toil_minutes_saved,
    count(*) FILTER (WHERE (al.outcome = ANY (ARRAY['reverted'::text, 'rolled_back'::text]))) AS actions_reverted,
    (0)::numeric AS potential_minutes_pending,
    (0)::bigint AS incidents_avoided,
    (0)::numeric AS incident_minutes_credited
   FROM sage.action_log al
  GROUP BY (date_trunc('day'::text, al.executed_at)), al.database_id, al.action_type
UNION ALL
 SELECT date_trunc('day'::text, aq.proposed_at) AS day,
    (aq.database_id)::bigint AS database_id,
    COALESCE(aq.action_type, 'unknown'::text) AS action_type,
    (0)::bigint AS actions_verified,
    (0)::numeric AS toil_minutes_saved,
    (0)::bigint AS actions_reverted,
    COALESCE(sum(tm.base_minutes), (0)::numeric) AS potential_minutes_pending,
    (0)::bigint AS incidents_avoided,
    (0)::numeric AS incident_minutes_credited
   FROM (sage.action_queue aq
     LEFT JOIN sage.toil_model tm ON (((tm.action_type = aq.action_type) AND (tm.effective_to IS NULL))))
  WHERE (aq.status = 'pending'::text)
  GROUP BY (date_trunc('day'::text, aq.proposed_at)), ((aq.database_id)::bigint), COALESCE(aq.action_type, 'unknown'::text)
UNION ALL
 SELECT date_trunc('day'::text, ia.occurred_at) AS day,
    ia.database_id,
    ('incident:'::text || ia.kind) AS action_type,
    (0)::bigint AS actions_verified,
    (0)::numeric AS toil_minutes_saved,
    (0)::bigint AS actions_reverted,
    (0)::numeric AS potential_minutes_pending,
    count(*) AS incidents_avoided,
    COALESCE(sum(ia.credited_minutes), (0)::numeric) AS incident_minutes_credited
   FROM sage.incident_avoided ia
  GROUP BY (date_trunc('day'::text, ia.occurred_at)), ia.database_id, ('incident:'::text || ia.kind);


--
-- Name: verification; Type: TABLE; Schema: sage; Owner: -
--

CREATE TABLE sage.verification (
    id bigint NOT NULL,
    database_id bigint,
    decision_id bigint NOT NULL,
    action_log_id bigint,
    criterion jsonb NOT NULL,
    baseline jsonb NOT NULL,
    minimum_samples integer NOT NULL,
    next_evaluation_at timestamp with time zone NOT NULL,
    hard_deadline_at timestamp with time zone NOT NULL,
    verdict text DEFAULT 'pending'::text NOT NULL,
    reason text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    CONSTRAINT verification_minimum_samples_check CHECK ((minimum_samples > 0)),
    CONSTRAINT verification_verdict_check CHECK ((verdict = ANY (ARRAY['pending'::text, 'extended'::text, 'success'::text, 'revert'::text, 'failed'::text, 'unverifiable'::text])))
)
WITH (autovacuum_vacuum_threshold='10', autovacuum_vacuum_scale_factor='0.05');


--
-- Name: verification_id_seq; Type: SEQUENCE; Schema: sage; Owner: -
--

CREATE SEQUENCE sage.verification_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;


--
-- Name: verification_id_seq; Type: SEQUENCE OWNED BY; Schema: sage; Owner: -
--

ALTER SEQUENCE sage.verification_id_seq OWNED BY sage.verification.id;


--
-- Name: query_store_default; Type: TABLE ATTACH; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.query_store ATTACH PARTITION sage.query_store_default DEFAULT;


--
-- Name: query_store_history; Type: TABLE ATTACH; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.query_store ATTACH PARTITION sage.query_store_history FOR VALUES FROM (MINVALUE) TO ('2026-11-17 00:00:00+00');


--
-- Name: query_store_p20261117; Type: TABLE ATTACH; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.query_store ATTACH PARTITION sage.query_store_p20261117 FOR VALUES FROM ('2026-11-17 00:00:00+00') TO ('2026-11-18 00:00:00+00');


--
-- Name: query_store_p20261118; Type: TABLE ATTACH; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.query_store ATTACH PARTITION sage.query_store_p20261118 FOR VALUES FROM ('2026-11-18 00:00:00+00') TO ('2026-11-19 00:00:00+00');


--
-- Name: snapshots_default; Type: TABLE ATTACH; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots ATTACH PARTITION sage.snapshots_default DEFAULT;


--
-- Name: snapshots_history; Type: TABLE ATTACH; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots ATTACH PARTITION sage.snapshots_history FOR VALUES FROM (MINVALUE) TO ('2026-11-17 00:00:00+00');


--
-- Name: snapshots_p20261117; Type: TABLE ATTACH; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots ATTACH PARTITION sage.snapshots_p20261117 FOR VALUES FROM ('2026-11-17 00:00:00+00') TO ('2026-11-18 00:00:00+00');


--
-- Name: snapshots_p20261118; Type: TABLE ATTACH; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots ATTACH PARTITION sage.snapshots_p20261118 FOR VALUES FROM ('2026-11-18 00:00:00+00') TO ('2026-11-19 00:00:00+00');


--
-- Name: action_log id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_log ALTER COLUMN id SET DEFAULT nextval('sage.action_log_id_seq'::regclass);


--
-- Name: action_queue id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_queue ALTER COLUMN id SET DEFAULT nextval('sage.action_queue_id_seq'::regclass);


--
-- Name: admission_withheld id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.admission_withheld ALTER COLUMN id SET DEFAULT nextval('sage.admission_withheld_id_seq'::regclass);


--
-- Name: alert_log id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.alert_log ALTER COLUMN id SET DEFAULT nextval('sage.alert_log_id_seq'::regclass);


--
-- Name: approval_card_deliveries id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.approval_card_deliveries ALTER COLUMN id SET DEFAULT nextval('sage.approval_card_deliveries_id_seq'::regclass);


--
-- Name: ask_messages id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.ask_messages ALTER COLUMN id SET DEFAULT nextval('sage.ask_messages_id_seq'::regclass);


--
-- Name: auth_audit id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.auth_audit ALTER COLUMN id SET DEFAULT nextval('sage.auth_audit_id_seq'::regclass);


--
-- Name: briefings id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.briefings ALTER COLUMN id SET DEFAULT nextval('sage.briefings_id_seq'::regclass);


--
-- Name: change_lease id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.change_lease ALTER COLUMN id SET DEFAULT nextval('sage.change_lease_id_seq'::regclass);


--
-- Name: chatops_identities id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.chatops_identities ALTER COLUMN id SET DEFAULT nextval('sage.chatops_identities_id_seq'::regclass);


--
-- Name: clone_instances id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.clone_instances ALTER COLUMN id SET DEFAULT nextval('sage.clone_instances_id_seq'::regclass);


--
-- Name: config_audit id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.config_audit ALTER COLUMN id SET DEFAULT nextval('sage.config_audit_id_seq'::regclass);


--
-- Name: config_derivation id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.config_derivation ALTER COLUMN id SET DEFAULT nextval('sage.config_derivation_id_seq'::regclass);


--
-- Name: databases id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.databases ALTER COLUMN id SET DEFAULT nextval('sage.databases_id_seq'::regclass);


--
-- Name: decision id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.decision ALTER COLUMN id SET DEFAULT nextval('sage.decision_id_seq'::regclass);


--
-- Name: explain_cache id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.explain_cache ALTER COLUMN id SET DEFAULT nextval('sage.explain_cache_id_seq'::regclass);


--
-- Name: fact_card_deliveries id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.fact_card_deliveries ALTER COLUMN id SET DEFAULT nextval('sage.fact_card_deliveries_id_seq'::regclass);


--
-- Name: facts id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.facts ALTER COLUMN id SET DEFAULT nextval('sage.facts_id_seq'::regclass);


--
-- Name: findings id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.findings ALTER COLUMN id SET DEFAULT nextval('sage.findings_id_seq'::regclass);


--
-- Name: first_look id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.first_look ALTER COLUMN id SET DEFAULT nextval('sage.first_look_id_seq'::regclass);


--
-- Name: guard_freezes id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_freezes ALTER COLUMN id SET DEFAULT nextval('sage.guard_freezes_id_seq'::regclass);


--
-- Name: guard_grant_requests id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_grant_requests ALTER COLUMN id SET DEFAULT nextval('sage.guard_grant_requests_id_seq'::regclass);


--
-- Name: guard_grants id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_grants ALTER COLUMN id SET DEFAULT nextval('sage.guard_grants_id_seq'::regclass);


--
-- Name: guard_kills id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_kills ALTER COLUMN id SET DEFAULT nextval('sage.guard_kills_id_seq'::regclass);


--
-- Name: guard_pgaudit_events id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_pgaudit_events ALTER COLUMN id SET DEFAULT nextval('sage.guard_pgaudit_events_id_seq'::regclass);


--
-- Name: guard_query_audit id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_query_audit ALTER COLUMN id SET DEFAULT nextval('sage.guard_query_audit_id_seq'::regclass);


--
-- Name: guard_unfreeze_requests id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_unfreeze_requests ALTER COLUMN id SET DEFAULT nextval('sage.guard_unfreeze_requests_id_seq'::regclass);


--
-- Name: health_history id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.health_history ALTER COLUMN id SET DEFAULT nextval('sage.health_history_id_seq'::regclass);


--
-- Name: incident_avoided id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.incident_avoided ALTER COLUMN id SET DEFAULT nextval('sage.incident_avoided_id_seq'::regclass);


--
-- Name: index_replace id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.index_replace ALTER COLUMN id SET DEFAULT nextval('sage.index_replace_id_seq'::regclass);


--
-- Name: io_rate_sample id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.io_rate_sample ALTER COLUMN id SET DEFAULT nextval('sage.io_rate_sample_id_seq'::regclass);


--
-- Name: lease_queue id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.lease_queue ALTER COLUMN id SET DEFAULT nextval('sage.lease_queue_id_seq'::regclass);


--
-- Name: managed_change_proposals id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.managed_change_proposals ALTER COLUMN id SET DEFAULT nextval('sage.managed_change_proposals_id_seq'::regclass);


--
-- Name: migration_run id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.migration_run ALTER COLUMN id SET DEFAULT nextval('sage.migration_run_id_seq'::regclass);


--
-- Name: notification_channels id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_channels ALTER COLUMN id SET DEFAULT nextval('sage.notification_channels_id_seq'::regclass);


--
-- Name: notification_log id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_log ALTER COLUMN id SET DEFAULT nextval('sage.notification_log_id_seq'::regclass);


--
-- Name: notification_rules id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_rules ALTER COLUMN id SET DEFAULT nextval('sage.notification_rules_id_seq'::regclass);


--
-- Name: optimizer_rejection id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.optimizer_rejection ALTER COLUMN id SET DEFAULT nextval('sage.optimizer_rejection_id_seq'::regclass);


--
-- Name: policy id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.policy ALTER COLUMN id SET DEFAULT nextval('sage.policy_id_seq'::regclass);


--
-- Name: query_hints id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.query_hints ALTER COLUMN id SET DEFAULT nextval('sage.query_hints_id_seq'::regclass);


--
-- Name: query_store id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.query_store ALTER COLUMN id SET DEFAULT nextval('sage.query_store_id_seq'::regclass);


--
-- Name: recommendation id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation ALTER COLUMN id SET DEFAULT nextval('sage.recommendation_id_seq'::regclass);


--
-- Name: recommendation_transition id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation_transition ALTER COLUMN id SET DEFAULT nextval('sage.recommendation_transition_id_seq'::regclass);


--
-- Name: retention_run id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.retention_run ALTER COLUMN id SET DEFAULT nextval('sage.retention_run_id_seq'::regclass);


--
-- Name: rollout_run id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.rollout_run ALTER COLUMN id SET DEFAULT nextval('sage.rollout_run_id_seq'::regclass);


--
-- Name: runway_samples id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.runway_samples ALTER COLUMN id SET DEFAULT nextval('sage.runway_samples_id_seq'::regclass);


--
-- Name: schema_baseline id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.schema_baseline ALTER COLUMN id SET DEFAULT nextval('sage.schema_baseline_id_seq'::regclass);


--
-- Name: shadow_decision id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.shadow_decision ALTER COLUMN id SET DEFAULT nextval('sage.shadow_decision_id_seq'::regclass);


--
-- Name: size_history id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.size_history ALTER COLUMN id SET DEFAULT nextval('sage.size_history_id_seq'::regclass);


--
-- Name: snapshots id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots ALTER COLUMN id SET DEFAULT nextval('sage.snapshots_id_seq'::regclass);


--
-- Name: source_fix id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.source_fix ALTER COLUMN id SET DEFAULT nextval('sage.source_fix_id_seq'::regclass);


--
-- Name: sre_autonomy_events id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_autonomy_events ALTER COLUMN id SET DEFAULT nextval('sage.sre_autonomy_events_id_seq'::regclass);


--
-- Name: sre_autonomy_outcomes id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_autonomy_outcomes ALTER COLUMN id SET DEFAULT nextval('sage.sre_autonomy_outcomes_id_seq'::regclass);


--
-- Name: table_contract id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.table_contract ALTER COLUMN id SET DEFAULT nextval('sage.table_contract_id_seq'::regclass);


--
-- Name: trust_shadow_evidence id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.trust_shadow_evidence ALTER COLUMN id SET DEFAULT nextval('sage.trust_shadow_evidence_id_seq'::regclass);


--
-- Name: user_oidc_link_grants id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.user_oidc_link_grants ALTER COLUMN id SET DEFAULT nextval('sage.user_oidc_link_grants_id_seq'::regclass);


--
-- Name: users id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.users ALTER COLUMN id SET DEFAULT nextval('sage.users_id_seq'::regclass);


--
-- Name: verification id; Type: DEFAULT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.verification ALTER COLUMN id SET DEFAULT nextval('sage.verification_id_seq'::regclass);


--
-- Data for Name: action_log; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: action_outcome; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: action_queue; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: admission_withheld; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: agentdb_decommission; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: alert_log; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: approval_card_deliveries; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: ask_budget_day; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: ask_conversations; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: ask_messages; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: audit_chain_link; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: audit_chain_meta; Type: TABLE DATA; Schema: sage; Owner: -
--

INSERT INTO sage.audit_chain_meta VALUES ('action_log', 'sage.action_log', 0, '2026-11-15 23:25:45.365524+00');
INSERT INTO sage.audit_chain_meta VALUES ('auth_audit', 'sage.auth_audit', 0, '2026-11-15 23:25:45.365524+00');
INSERT INTO sage.audit_chain_meta VALUES ('config_audit', 'sage.config_audit', 0, '2026-11-15 23:25:45.365524+00');
INSERT INTO sage.audit_chain_meta VALUES ('guard_query_audit', 'sage.guard_query_audit', 0, '2026-11-15 23:25:45.365524+00');
INSERT INTO sage.audit_chain_meta VALUES ('pgaudit_events', 'sage.guard_pgaudit_events', 0, '2026-11-15 23:25:45.365524+00');


--
-- Data for Name: auth_audit; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: briefings; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: change_lease; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: chatops_identities; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: chatops_replay; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: clone_instances; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: config; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: config_audit; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: config_derivation; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: config_derived_setting; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: crypto_meta; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: databases; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: decision; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: explain_cache; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: explain_results; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: fact_card_deliveries; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: facts; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: findings; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: first_look; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: fleet_fingerprint; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: fleet_leader_lease; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: fleet_outcome_digest; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_cluster_roles; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_environment_labels; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_freezes; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_grant_requests; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_grants; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_identity_bindings; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_inflight; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_kills; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_pgaudit_events; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_principal_files; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_principals; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_public_baseline; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_query_audit; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_taint; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: guard_unfreeze_requests; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: ha_identity; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: health_history; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: incident_avoided; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: incidents; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: index_replace; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: io_rate_sample; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: lease_queue; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: managed_change_proposals; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: mcp_tokens; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: migration_run; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: notification_channels; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: notification_log; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: notification_rules; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: onboarding; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: optimizer_rejection; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: policy; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: policy_approvals; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: query_hints; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: query_store_default; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: query_store_history; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: query_store_p20261117; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: query_store_p20261118; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: recommendation; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: recommendation_revision; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: recommendation_transition; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: retention_run; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: rollout_instance; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: rollout_run; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: runway_samples; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: schema_baseline; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: schema_findings; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sessions; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: shadow_decision; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: siem_cursor; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: size_history; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: slot_consumer_registry; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: snapshots_default; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: snapshots_history; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: snapshots_p20261117; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: snapshots_p20261118; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: source_fix; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: specialist_requests; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_action_proposals; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_autonomy_events; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_autonomy_outcomes; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_autonomy_proposals; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_budget_reservations; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_change_events; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_change_feed_state; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_database_bindings; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_deployments; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_eval_runs; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_events; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_evidence; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_family_autonomy; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_game_days; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_hypotheses; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_investigation_outcomes; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_investigations; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_maintenance_proposals; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_packet_reviews; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_runbook_runs; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_runbook_versions; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_runbooks; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_service_slos; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_sli_samples; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_slo_transitions; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_steps; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: sre_tombstones; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: table_contract; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: toil_model; Type: TABLE DATA; Schema: sage; Owner: -
--

INSERT INTO sage.toil_model VALUES ('analyze_table', 1, 15, 'Analyze and validate table statistics', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('create_index_concurrently', 1, 45, 'Analyze, build, and verify index', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('drop_unused_index', 1, 20, 'Prove disuse, remove, and verify', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('reindex_concurrently', 1, 30, 'Plan, rebuild, and verify index', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('vacuum_table', 1, 20, 'Diagnose and vacuum table', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('freeze_table', 1, 20, 'Diagnose and freeze table', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('set_table_autovacuum', 1, 30, 'Tune and verify table autovacuum', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('alter_system_guc', 1, 25, 'Assess, apply, and verify configuration', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('create_statistics', 1, 30, 'Analyze and create extended statistics', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('apply_query_hint', 1, 25, 'Assess, apply, and verify query hint', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('online_migration', 1, 120, 'Plan, rehearse, apply, and verify migration', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('fk_supporting_index', 1, 30, 'Detect and build foreign-key index', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('retention_policy_setup', 1, 60, 'Design and verify retention policy', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('slot_bound', 1, 30, 'Diagnose and bound retained WAL', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);
INSERT INTO sage.toil_model VALUES ('slot_drop', 1, 30, 'Prove abandonment and remove slot', 'phase0', '2026-11-15 23:25:42.874324+00', NULL);


--
-- Data for Name: trust_ledger_state; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: trust_shadow_evidence; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: tuning_budget_day; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: user_oidc_link_grants; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: users; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Data for Name: verification; Type: TABLE DATA; Schema: sage; Owner: -
--



--
-- Name: action_log_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.action_log_id_seq', 1, false);


--
-- Name: action_queue_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.action_queue_id_seq', 1, false);


--
-- Name: admission_withheld_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.admission_withheld_id_seq', 1, false);


--
-- Name: alert_log_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.alert_log_id_seq', 1, false);


--
-- Name: approval_card_deliveries_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.approval_card_deliveries_id_seq', 1, false);


--
-- Name: ask_messages_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.ask_messages_id_seq', 1, false);


--
-- Name: auth_audit_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.auth_audit_id_seq', 1, false);


--
-- Name: briefings_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.briefings_id_seq', 1, false);


--
-- Name: change_lease_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.change_lease_id_seq', 1, false);


--
-- Name: chatops_identities_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.chatops_identities_id_seq', 1, false);


--
-- Name: clone_instances_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.clone_instances_id_seq', 1, false);


--
-- Name: config_audit_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.config_audit_id_seq', 1, false);


--
-- Name: config_derivation_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.config_derivation_id_seq', 1, false);


--
-- Name: databases_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.databases_id_seq', 1, false);


--
-- Name: decision_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.decision_id_seq', 1, false);


--
-- Name: explain_cache_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.explain_cache_id_seq', 1, false);


--
-- Name: fact_card_deliveries_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.fact_card_deliveries_id_seq', 1, false);


--
-- Name: facts_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.facts_id_seq', 1, false);


--
-- Name: findings_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.findings_id_seq', 1, false);


--
-- Name: first_look_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.first_look_id_seq', 1, false);


--
-- Name: guard_freezes_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.guard_freezes_id_seq', 1, false);


--
-- Name: guard_grant_requests_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.guard_grant_requests_id_seq', 1, false);


--
-- Name: guard_grants_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.guard_grants_id_seq', 1, false);


--
-- Name: guard_kills_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.guard_kills_id_seq', 1, false);


--
-- Name: guard_pgaudit_events_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.guard_pgaudit_events_id_seq', 1, false);


--
-- Name: guard_query_audit_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.guard_query_audit_id_seq', 1, false);


--
-- Name: guard_unfreeze_requests_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.guard_unfreeze_requests_id_seq', 1, false);


--
-- Name: health_history_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.health_history_id_seq', 1, false);


--
-- Name: incident_avoided_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.incident_avoided_id_seq', 1, false);


--
-- Name: index_replace_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.index_replace_id_seq', 1, false);


--
-- Name: io_rate_sample_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.io_rate_sample_id_seq', 1, false);


--
-- Name: lease_queue_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.lease_queue_id_seq', 1, false);


--
-- Name: managed_change_proposals_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.managed_change_proposals_id_seq', 1, false);


--
-- Name: migration_run_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.migration_run_id_seq', 1, false);


--
-- Name: notification_channels_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.notification_channels_id_seq', 1, false);


--
-- Name: notification_log_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.notification_log_id_seq', 1, false);


--
-- Name: notification_rules_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.notification_rules_id_seq', 1, false);


--
-- Name: optimizer_rejection_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.optimizer_rejection_id_seq', 1, false);


--
-- Name: policy_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.policy_id_seq', 1, false);


--
-- Name: query_hints_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.query_hints_id_seq', 1, false);


--
-- Name: query_store_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.query_store_id_seq', 1, false);


--
-- Name: recommendation_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.recommendation_id_seq', 1, false);


--
-- Name: recommendation_transition_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.recommendation_transition_id_seq', 1, false);


--
-- Name: retention_run_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.retention_run_id_seq', 1, false);


--
-- Name: rollout_run_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.rollout_run_id_seq', 1, false);


--
-- Name: runway_samples_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.runway_samples_id_seq', 1, false);


--
-- Name: schema_baseline_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.schema_baseline_id_seq', 1, false);


--
-- Name: shadow_decision_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.shadow_decision_id_seq', 1, false);


--
-- Name: size_history_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.size_history_id_seq', 1, false);


--
-- Name: snapshots_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.snapshots_id_seq', 1, false);


--
-- Name: source_fix_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.source_fix_id_seq', 1, false);


--
-- Name: sre_autonomy_events_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.sre_autonomy_events_id_seq', 1, false);


--
-- Name: sre_autonomy_outcomes_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.sre_autonomy_outcomes_id_seq', 1, false);


--
-- Name: sre_slo_transitions_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.sre_slo_transitions_id_seq', 1, false);


--
-- Name: table_contract_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.table_contract_id_seq', 1, false);


--
-- Name: trust_shadow_evidence_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.trust_shadow_evidence_id_seq', 1, false);


--
-- Name: user_oidc_link_grants_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.user_oidc_link_grants_id_seq', 1, false);


--
-- Name: users_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.users_id_seq', 1, false);


--
-- Name: verification_id_seq; Type: SEQUENCE SET; Schema: sage; Owner: -
--

SELECT pg_catalog.setval('sage.verification_id_seq', 1, false);


--
-- Name: action_log action_log_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_log
    ADD CONSTRAINT action_log_pkey PRIMARY KEY (id);


--
-- Name: action_outcome action_outcome_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_outcome
    ADD CONSTRAINT action_outcome_pkey PRIMARY KEY (action_log_id);


--
-- Name: action_queue action_queue_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_queue
    ADD CONSTRAINT action_queue_pkey PRIMARY KEY (id);


--
-- Name: admission_withheld admission_withheld_database_name_finding_key_reason_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.admission_withheld
    ADD CONSTRAINT admission_withheld_database_name_finding_key_reason_key UNIQUE (database_name, finding_key, reason);


--
-- Name: admission_withheld admission_withheld_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.admission_withheld
    ADD CONSTRAINT admission_withheld_pkey PRIMARY KEY (id);


--
-- Name: agentdb_decommission agentdb_decommission_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.agentdb_decommission
    ADD CONSTRAINT agentdb_decommission_pkey PRIMARY KEY (resource_id);


--
-- Name: alert_log alert_log_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.alert_log
    ADD CONSTRAINT alert_log_pkey PRIMARY KEY (id);


--
-- Name: approval_card_deliveries approval_card_deliveries_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.approval_card_deliveries
    ADD CONSTRAINT approval_card_deliveries_pkey PRIMARY KEY (id);


--
-- Name: approval_card_deliveries approval_card_deliveries_token_sha256_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.approval_card_deliveries
    ADD CONSTRAINT approval_card_deliveries_token_sha256_key UNIQUE (token_sha256);


--
-- Name: ask_budget_day ask_budget_day_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.ask_budget_day
    ADD CONSTRAINT ask_budget_day_pkey PRIMARY KEY (day, actor);


--
-- Name: ask_conversations ask_conversations_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.ask_conversations
    ADD CONSTRAINT ask_conversations_pkey PRIMARY KEY (id);


--
-- Name: ask_messages ask_messages_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.ask_messages
    ADD CONSTRAINT ask_messages_pkey PRIMARY KEY (id);


--
-- Name: audit_chain_link audit_chain_link_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.audit_chain_link
    ADD CONSTRAINT audit_chain_link_pkey PRIMARY KEY (chain, seq);


--
-- Name: audit_chain_meta audit_chain_meta_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.audit_chain_meta
    ADD CONSTRAINT audit_chain_meta_pkey PRIMARY KEY (chain);


--
-- Name: auth_audit auth_audit_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.auth_audit
    ADD CONSTRAINT auth_audit_pkey PRIMARY KEY (id);


--
-- Name: briefings briefings_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.briefings
    ADD CONSTRAINT briefings_pkey PRIMARY KEY (id);


--
-- Name: change_lease change_lease_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.change_lease
    ADD CONSTRAINT change_lease_pkey PRIMARY KEY (id);


--
-- Name: chatops_identities chatops_identities_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.chatops_identities
    ADD CONSTRAINT chatops_identities_pkey PRIMARY KEY (id);


--
-- Name: chatops_identities chatops_identities_provider_team_id_external_user_id_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.chatops_identities
    ADD CONSTRAINT chatops_identities_provider_team_id_external_user_id_key UNIQUE (provider, team_id, external_user_id);


--
-- Name: chatops_replay chatops_replay_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.chatops_replay
    ADD CONSTRAINT chatops_replay_pkey PRIMARY KEY (provider, nonce);


--
-- Name: clone_instances clone_instances_adapter_scope_name_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.clone_instances
    ADD CONSTRAINT clone_instances_adapter_scope_name_key UNIQUE (adapter, scope, name);


--
-- Name: clone_instances clone_instances_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.clone_instances
    ADD CONSTRAINT clone_instances_pkey PRIMARY KEY (id);


--
-- Name: config_audit config_audit_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.config_audit
    ADD CONSTRAINT config_audit_pkey PRIMARY KEY (id);


--
-- Name: config_derivation config_derivation_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.config_derivation
    ADD CONSTRAINT config_derivation_pkey PRIMARY KEY (id);


--
-- Name: config_derived_setting config_derived_setting_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.config_derived_setting
    ADD CONSTRAINT config_derived_setting_pkey PRIMARY KEY (key);


--
-- Name: crypto_meta crypto_meta_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.crypto_meta
    ADD CONSTRAINT crypto_meta_pkey PRIMARY KEY (id);


--
-- Name: databases databases_name_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.databases
    ADD CONSTRAINT databases_name_key UNIQUE (name);


--
-- Name: databases databases_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.databases
    ADD CONSTRAINT databases_pkey PRIMARY KEY (id);


--
-- Name: decision decision_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.decision
    ADD CONSTRAINT decision_pkey PRIMARY KEY (id);


--
-- Name: explain_cache explain_cache_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.explain_cache
    ADD CONSTRAINT explain_cache_pkey PRIMARY KEY (id);


--
-- Name: explain_results explain_results_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.explain_results
    ADD CONSTRAINT explain_results_pkey PRIMARY KEY (query_hash, database_name);


--
-- Name: fact_card_deliveries fact_card_deliveries_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.fact_card_deliveries
    ADD CONSTRAINT fact_card_deliveries_pkey PRIMARY KEY (id);


--
-- Name: fact_card_deliveries fact_card_deliveries_token_sha256_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.fact_card_deliveries
    ADD CONSTRAINT fact_card_deliveries_token_sha256_key UNIQUE (token_sha256);


--
-- Name: facts facts_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.facts
    ADD CONSTRAINT facts_pkey PRIMARY KEY (id);


--
-- Name: findings findings_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.findings
    ADD CONSTRAINT findings_pkey PRIMARY KEY (id);


--
-- Name: first_look first_look_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.first_look
    ADD CONSTRAINT first_look_pkey PRIMARY KEY (id);


--
-- Name: fleet_fingerprint fleet_fingerprint_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.fleet_fingerprint
    ADD CONSTRAINT fleet_fingerprint_pkey PRIMARY KEY (fleet_scope, database_name);


--
-- Name: fleet_leader_lease fleet_leader_lease_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.fleet_leader_lease
    ADD CONSTRAINT fleet_leader_lease_pkey PRIMARY KEY (scope);


--
-- Name: fleet_outcome_digest fleet_outcome_digest_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.fleet_outcome_digest
    ADD CONSTRAINT fleet_outcome_digest_pkey PRIMARY KEY (fleet_scope, database_name, action_class, shape);


--
-- Name: guard_cluster_roles guard_cluster_roles_cluster_key_broker_role_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_cluster_roles
    ADD CONSTRAINT guard_cluster_roles_cluster_key_broker_role_key UNIQUE (cluster_key, broker_role);


--
-- Name: guard_cluster_roles guard_cluster_roles_cluster_key_login_role_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_cluster_roles
    ADD CONSTRAINT guard_cluster_roles_cluster_key_login_role_key UNIQUE (cluster_key, login_role);


--
-- Name: guard_cluster_roles guard_cluster_roles_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_cluster_roles
    ADD CONSTRAINT guard_cluster_roles_pkey PRIMARY KEY (principal_id, cluster_key);


--
-- Name: guard_environment_labels guard_environment_labels_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_environment_labels
    ADD CONSTRAINT guard_environment_labels_pkey PRIMARY KEY (database_id);


--
-- Name: guard_freezes guard_freezes_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_freezes
    ADD CONSTRAINT guard_freezes_pkey PRIMARY KEY (id);


--
-- Name: guard_grant_requests guard_grant_requests_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_grant_requests
    ADD CONSTRAINT guard_grant_requests_pkey PRIMARY KEY (id);


--
-- Name: guard_grants guard_grants_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_grants
    ADD CONSTRAINT guard_grants_pkey PRIMARY KEY (id);


--
-- Name: guard_identity_bindings guard_identity_bindings_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_identity_bindings
    ADD CONSTRAINT guard_identity_bindings_pkey PRIMARY KEY (issuer, subject);


--
-- Name: guard_inflight guard_inflight_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_inflight
    ADD CONSTRAINT guard_inflight_pkey PRIMARY KEY (database_id, backend_pid);


--
-- Name: guard_kills guard_kills_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_kills
    ADD CONSTRAINT guard_kills_pkey PRIMARY KEY (id);


--
-- Name: guard_pgaudit_events guard_pgaudit_events_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_pgaudit_events
    ADD CONSTRAINT guard_pgaudit_events_pkey PRIMARY KEY (id);


--
-- Name: guard_principal_files guard_principal_files_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_principal_files
    ADD CONSTRAINT guard_principal_files_pkey PRIMARY KEY (principal_id);


--
-- Name: guard_principals guard_principals_name_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_principals
    ADD CONSTRAINT guard_principals_name_key UNIQUE (name);


--
-- Name: guard_principals guard_principals_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_principals
    ADD CONSTRAINT guard_principals_pkey PRIMARY KEY (id);


--
-- Name: guard_public_baseline guard_public_baseline_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_public_baseline
    ADD CONSTRAINT guard_public_baseline_pkey PRIMARY KEY (object_kind, object_oid, attnum, privilege);


--
-- Name: guard_query_audit guard_query_audit_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_query_audit
    ADD CONSTRAINT guard_query_audit_pkey PRIMARY KEY (id);


--
-- Name: guard_taint guard_taint_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_taint
    ADD CONSTRAINT guard_taint_pkey PRIMARY KEY (principal_id, task_id, source);


--
-- Name: guard_unfreeze_requests guard_unfreeze_requests_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_unfreeze_requests
    ADD CONSTRAINT guard_unfreeze_requests_pkey PRIMARY KEY (id);


--
-- Name: ha_identity ha_identity_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.ha_identity
    ADD CONSTRAINT ha_identity_pkey PRIMARY KEY (monitor_key);


--
-- Name: health_history health_history_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.health_history
    ADD CONSTRAINT health_history_pkey PRIMARY KEY (id);


--
-- Name: incident_avoided incident_avoided_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.incident_avoided
    ADD CONSTRAINT incident_avoided_pkey PRIMARY KEY (id);


--
-- Name: incidents incidents_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.incidents
    ADD CONSTRAINT incidents_pkey PRIMARY KEY (id);


--
-- Name: index_replace index_replace_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.index_replace
    ADD CONSTRAINT index_replace_pkey PRIMARY KEY (id);


--
-- Name: io_rate_sample io_rate_sample_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.io_rate_sample
    ADD CONSTRAINT io_rate_sample_pkey PRIMARY KEY (id);


--
-- Name: lease_queue lease_queue_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.lease_queue
    ADD CONSTRAINT lease_queue_pkey PRIMARY KEY (id);


--
-- Name: managed_change_proposals managed_change_proposals_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.managed_change_proposals
    ADD CONSTRAINT managed_change_proposals_pkey PRIMARY KEY (id);


--
-- Name: mcp_tokens mcp_tokens_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.mcp_tokens
    ADD CONSTRAINT mcp_tokens_pkey PRIMARY KEY (id);


--
-- Name: mcp_tokens mcp_tokens_token_hash_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.mcp_tokens
    ADD CONSTRAINT mcp_tokens_token_hash_key UNIQUE (token_hash);


--
-- Name: migration_run migration_run_evidence_id_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.migration_run
    ADD CONSTRAINT migration_run_evidence_id_key UNIQUE (evidence_id);


--
-- Name: migration_run migration_run_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.migration_run
    ADD CONSTRAINT migration_run_pkey PRIMARY KEY (id);


--
-- Name: notification_channels notification_channels_name_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_channels
    ADD CONSTRAINT notification_channels_name_key UNIQUE (name);


--
-- Name: notification_channels notification_channels_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_channels
    ADD CONSTRAINT notification_channels_pkey PRIMARY KEY (id);


--
-- Name: notification_log notification_log_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_log
    ADD CONSTRAINT notification_log_pkey PRIMARY KEY (id);


--
-- Name: notification_rules notification_rules_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_rules
    ADD CONSTRAINT notification_rules_pkey PRIMARY KEY (id);


--
-- Name: onboarding onboarding_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.onboarding
    ADD CONSTRAINT onboarding_pkey PRIMARY KEY (database_name);


--
-- Name: optimizer_rejection optimizer_rejection_database_name_schema_name_table_name_sh_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.optimizer_rejection
    ADD CONSTRAINT optimizer_rejection_database_name_schema_name_table_name_sh_key UNIQUE (database_name, schema_name, table_name, shape_hash);


--
-- Name: optimizer_rejection optimizer_rejection_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.optimizer_rejection
    ADD CONSTRAINT optimizer_rejection_pkey PRIMARY KEY (id);


--
-- Name: policy_approvals policy_approvals_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.policy_approvals
    ADD CONSTRAINT policy_approvals_pkey PRIMARY KEY (policy_id, approver);


--
-- Name: policy policy_database_id_version_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.policy
    ADD CONSTRAINT policy_database_id_version_key UNIQUE (database_id, version);


--
-- Name: policy policy_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.policy
    ADD CONSTRAINT policy_pkey PRIMARY KEY (id);


--
-- Name: query_hints query_hints_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.query_hints
    ADD CONSTRAINT query_hints_pkey PRIMARY KEY (id);


--
-- Name: recommendation recommendation_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation
    ADD CONSTRAINT recommendation_pkey PRIMARY KEY (id);


--
-- Name: recommendation_revision recommendation_revision_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation_revision
    ADD CONSTRAINT recommendation_revision_pkey PRIMARY KEY (recommendation_id, revision);


--
-- Name: recommendation_transition recommendation_transition_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation_transition
    ADD CONSTRAINT recommendation_transition_pkey PRIMARY KEY (id);


--
-- Name: retention_run retention_run_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.retention_run
    ADD CONSTRAINT retention_run_pkey PRIMARY KEY (id);


--
-- Name: rollout_instance rollout_instance_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.rollout_instance
    ADD CONSTRAINT rollout_instance_pkey PRIMARY KEY (run_evidence_id, instance_id);


--
-- Name: rollout_run rollout_run_evidence_id_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.rollout_run
    ADD CONSTRAINT rollout_run_evidence_id_key UNIQUE (evidence_id);


--
-- Name: rollout_run rollout_run_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.rollout_run
    ADD CONSTRAINT rollout_run_pkey PRIMARY KEY (id);


--
-- Name: runway_samples runway_samples_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.runway_samples
    ADD CONSTRAINT runway_samples_pkey PRIMARY KEY (id);


--
-- Name: schema_baseline schema_baseline_database_id_object_identity_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.schema_baseline
    ADD CONSTRAINT schema_baseline_database_id_object_identity_key UNIQUE (database_id, object_identity);


--
-- Name: schema_baseline schema_baseline_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.schema_baseline
    ADD CONSTRAINT schema_baseline_pkey PRIMARY KEY (id);


--
-- Name: schema_findings schema_findings_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.schema_findings
    ADD CONSTRAINT schema_findings_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: shadow_decision shadow_decision_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.shadow_decision
    ADD CONSTRAINT shadow_decision_pkey PRIMARY KEY (id);


--
-- Name: siem_cursor siem_cursor_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.siem_cursor
    ADD CONSTRAINT siem_cursor_pkey PRIMARY KEY (sink, source, chain);


--
-- Name: size_history size_history_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.size_history
    ADD CONSTRAINT size_history_pkey PRIMARY KEY (id);


--
-- Name: slot_consumer_registry slot_consumer_registry_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.slot_consumer_registry
    ADD CONSTRAINT slot_consumer_registry_pkey PRIMARY KEY (slot_name);


--
-- Name: snapshots snapshots_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots
    ADD CONSTRAINT snapshots_pkey PRIMARY KEY (id, collected_at);


--
-- Name: snapshots_default snapshots_default_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots_default
    ADD CONSTRAINT snapshots_default_pkey PRIMARY KEY (id, collected_at);


--
-- Name: snapshots_history snapshots_history_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots_history
    ADD CONSTRAINT snapshots_history_pkey PRIMARY KEY (id, collected_at);


--
-- Name: snapshots_p20261117 snapshots_p20261117_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots_p20261117
    ADD CONSTRAINT snapshots_p20261117_pkey PRIMARY KEY (id, collected_at);


--
-- Name: snapshots_p20261118 snapshots_p20261118_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.snapshots_p20261118
    ADD CONSTRAINT snapshots_p20261118_pkey PRIMARY KEY (id, collected_at);


--
-- Name: source_fix source_fix_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.source_fix
    ADD CONSTRAINT source_fix_pkey PRIMARY KEY (id);


--
-- Name: specialist_requests specialist_requests_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.specialist_requests
    ADD CONSTRAINT specialist_requests_pkey PRIMARY KEY (id);


--
-- Name: sre_action_proposals sre_action_proposals_deployment_id_database_id_investigatio_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_action_proposals
    ADD CONSTRAINT sre_action_proposals_deployment_id_database_id_investigatio_key UNIQUE (deployment_id, database_id, investigation_id, action_class);


--
-- Name: sre_action_proposals sre_action_proposals_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_action_proposals
    ADD CONSTRAINT sre_action_proposals_pkey PRIMARY KEY (id);


--
-- Name: sre_autonomy_events sre_autonomy_events_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_autonomy_events
    ADD CONSTRAINT sre_autonomy_events_pkey PRIMARY KEY (id);


--
-- Name: sre_autonomy_outcomes sre_autonomy_outcomes_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_autonomy_outcomes
    ADD CONSTRAINT sre_autonomy_outcomes_pkey PRIMARY KEY (id);


--
-- Name: sre_autonomy_proposals sre_autonomy_proposals_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_autonomy_proposals
    ADD CONSTRAINT sre_autonomy_proposals_pkey PRIMARY KEY (deployment_id, id);


--
-- Name: sre_budget_reservations sre_budget_reservations_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_budget_reservations
    ADD CONSTRAINT sre_budget_reservations_pkey PRIMARY KEY (deployment_id, database_id, id);


--
-- Name: sre_change_events sre_change_events_deployment_id_source_event_id_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_change_events
    ADD CONSTRAINT sre_change_events_deployment_id_source_event_id_key UNIQUE (deployment_id, source, event_id);


--
-- Name: sre_change_events sre_change_events_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_change_events
    ADD CONSTRAINT sre_change_events_pkey PRIMARY KEY (deployment_id, id);


--
-- Name: sre_change_feed_state sre_change_feed_state_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_change_feed_state
    ADD CONSTRAINT sre_change_feed_state_pkey PRIMARY KEY (deployment_id, database_id, source);


--
-- Name: sre_database_bindings sre_database_bindings_deployment_id_runtime_key_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_database_bindings
    ADD CONSTRAINT sre_database_bindings_deployment_id_runtime_key_key UNIQUE (deployment_id, runtime_key);


--
-- Name: sre_database_bindings sre_database_bindings_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_database_bindings
    ADD CONSTRAINT sre_database_bindings_pkey PRIMARY KEY (deployment_id, database_id);


--
-- Name: sre_deployments sre_deployments_deployment_id_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_deployments
    ADD CONSTRAINT sre_deployments_deployment_id_key UNIQUE (deployment_id);


--
-- Name: sre_deployments sre_deployments_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_deployments
    ADD CONSTRAINT sre_deployments_pkey PRIMARY KEY (singleton);


--
-- Name: sre_eval_runs sre_eval_runs_deployment_id_report_sha256_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_eval_runs
    ADD CONSTRAINT sre_eval_runs_deployment_id_report_sha256_key UNIQUE (deployment_id, report_sha256);


--
-- Name: sre_eval_runs sre_eval_runs_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_eval_runs
    ADD CONSTRAINT sre_eval_runs_pkey PRIMARY KEY (deployment_id, id);


--
-- Name: sre_events sre_events_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_events
    ADD CONSTRAINT sre_events_pkey PRIMARY KEY (deployment_id, database_id, investigation_id, sequence);


--
-- Name: sre_evidence sre_evidence_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_evidence
    ADD CONSTRAINT sre_evidence_pkey PRIMARY KEY (deployment_id, database_id, investigation_id, id);


--
-- Name: sre_family_autonomy sre_family_autonomy_db_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_family_autonomy
    ADD CONSTRAINT sre_family_autonomy_db_pkey PRIMARY KEY (deployment_id, database_name, family, action_class);


--
-- Name: sre_game_days sre_game_days_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_game_days
    ADD CONSTRAINT sre_game_days_pkey PRIMARY KEY (deployment_id, id);


--
-- Name: sre_hypotheses sre_hypotheses_deployment_id_database_id_investigation_id_r_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_hypotheses
    ADD CONSTRAINT sre_hypotheses_deployment_id_database_id_investigation_id_r_key UNIQUE (deployment_id, database_id, investigation_id, revision, ordinal);


--
-- Name: sre_hypotheses sre_hypotheses_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_hypotheses
    ADD CONSTRAINT sre_hypotheses_pkey PRIMARY KEY (deployment_id, database_id, investigation_id, id);


--
-- Name: sre_investigation_outcomes sre_investigation_outcomes_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_investigation_outcomes
    ADD CONSTRAINT sre_investigation_outcomes_pkey PRIMARY KEY (deployment_id, database_id, investigation_id, id);


--
-- Name: sre_investigations sre_investigations_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_investigations
    ADD CONSTRAINT sre_investigations_pkey PRIMARY KEY (deployment_id, database_id, id);


--
-- Name: sre_maintenance_proposals sre_maintenance_proposals_deployment_id_database_id_investi_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_maintenance_proposals
    ADD CONSTRAINT sre_maintenance_proposals_deployment_id_database_id_investi_key UNIQUE (deployment_id, database_id, investigation_id);


--
-- Name: sre_maintenance_proposals sre_maintenance_proposals_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_maintenance_proposals
    ADD CONSTRAINT sre_maintenance_proposals_pkey PRIMARY KEY (id);


--
-- Name: sre_packet_reviews sre_packet_reviews_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_packet_reviews
    ADD CONSTRAINT sre_packet_reviews_pkey PRIMARY KEY (deployment_id, database_name, investigation_id);


--
-- Name: sre_runbook_runs sre_runbook_runs_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_runbook_runs
    ADD CONSTRAINT sre_runbook_runs_pkey PRIMARY KEY (deployment_id, database_id, investigation_id, runbook_id, version);


--
-- Name: sre_runbook_versions sre_runbook_versions_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_runbook_versions
    ADD CONSTRAINT sre_runbook_versions_pkey PRIMARY KEY (deployment_id, database_id, runbook_id, version);


--
-- Name: sre_runbooks sre_runbooks_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_runbooks
    ADD CONSTRAINT sre_runbooks_pkey PRIMARY KEY (deployment_id, database_id, id);


--
-- Name: sre_service_slos sre_service_slos_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_service_slos
    ADD CONSTRAINT sre_service_slos_pkey PRIMARY KEY (deployment_id, database_id, name);


--
-- Name: sre_sli_samples sre_sli_samples_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_sli_samples
    ADD CONSTRAINT sre_sli_samples_pkey PRIMARY KEY (deployment_id, slo_name, series, observed_at);


--
-- Name: sre_slo_transitions sre_slo_transitions_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_slo_transitions
    ADD CONSTRAINT sre_slo_transitions_pkey PRIMARY KEY (id);


--
-- Name: sre_steps sre_steps_deployment_id_database_id_investigation_id_idempo_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_steps
    ADD CONSTRAINT sre_steps_deployment_id_database_id_investigation_id_idempo_key UNIQUE (deployment_id, database_id, investigation_id, idempotency_key);


--
-- Name: sre_steps sre_steps_deployment_id_database_id_investigation_id_sequen_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_steps
    ADD CONSTRAINT sre_steps_deployment_id_database_id_investigation_id_sequen_key UNIQUE (deployment_id, database_id, investigation_id, sequence);


--
-- Name: sre_steps sre_steps_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_steps
    ADD CONSTRAINT sre_steps_pkey PRIMARY KEY (deployment_id, database_id, investigation_id, id);


--
-- Name: sre_tombstones sre_tombstones_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_tombstones
    ADD CONSTRAINT sre_tombstones_pkey PRIMARY KEY (deployment_id, database_id, investigation_id, kind);


--
-- Name: table_contract table_contract_database_id_schema_name_table_name_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.table_contract
    ADD CONSTRAINT table_contract_database_id_schema_name_table_name_key UNIQUE (database_id, schema_name, table_name);


--
-- Name: table_contract table_contract_evidence_id_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.table_contract
    ADD CONSTRAINT table_contract_evidence_id_key UNIQUE (evidence_id);


--
-- Name: table_contract table_contract_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.table_contract
    ADD CONSTRAINT table_contract_pkey PRIMARY KEY (id);


--
-- Name: toil_model toil_model_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.toil_model
    ADD CONSTRAINT toil_model_pkey PRIMARY KEY (action_type, model_version);


--
-- Name: trust_ledger_state trust_ledger_state_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.trust_ledger_state
    ADD CONSTRAINT trust_ledger_state_pkey PRIMARY KEY (deployment_id, database_name);


--
-- Name: trust_shadow_evidence trust_shadow_evidence_deployment_id_database_name_shadow_id_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.trust_shadow_evidence
    ADD CONSTRAINT trust_shadow_evidence_deployment_id_database_name_shadow_id_key UNIQUE (deployment_id, database_name, shadow_id);


--
-- Name: trust_shadow_evidence trust_shadow_evidence_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.trust_shadow_evidence
    ADD CONSTRAINT trust_shadow_evidence_pkey PRIMARY KEY (id);


--
-- Name: tuning_budget_day tuning_budget_day_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.tuning_budget_day
    ADD CONSTRAINT tuning_budget_day_pkey PRIMARY KEY (database_name, utc_day);


--
-- Name: user_oidc_link_grants user_oidc_link_grants_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.user_oidc_link_grants
    ADD CONSTRAINT user_oidc_link_grants_pkey PRIMARY KEY (id);


--
-- Name: user_oidc_link_grants user_oidc_link_grants_token_hash_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.user_oidc_link_grants
    ADD CONSTRAINT user_oidc_link_grants_token_hash_key UNIQUE (token_hash);


--
-- Name: users users_email_key; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.users
    ADD CONSTRAINT users_email_key UNIQUE (email);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: verification verification_pkey; Type: CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.verification
    ADD CONSTRAINT verification_pkey PRIMARY KEY (id);


--
-- Name: action_queue_principal_open; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX action_queue_principal_open ON sage.action_queue USING btree (principal_id) WHERE ((principal_id IS NOT NULL) AND (status = ANY (ARRAY['pending'::text, 'approved'::text])));


--
-- Name: action_queue_principal_status; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX action_queue_principal_status ON sage.action_queue USING btree (principal_id, status) WHERE (principal_id IS NOT NULL);


--
-- Name: ask_conversations_actor_updated; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX ask_conversations_actor_updated ON sage.ask_conversations USING btree (actor, updated_at DESC);


--
-- Name: ask_conversations_updated; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX ask_conversations_updated ON sage.ask_conversations USING btree (updated_at);


--
-- Name: ask_messages_conversation; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX ask_messages_conversation ON sage.ask_messages USING btree (conversation_id, id);


--
-- Name: audit_chain_link_row; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX audit_chain_link_row ON sage.audit_chain_link USING btree (chain, row_id, seq);


--
-- Name: audit_chain_link_truncate; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX audit_chain_link_truncate ON sage.audit_chain_link USING btree (chain, seq) WHERE (op = 'T'::text);


--
-- Name: clone_instances_ready_name; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX clone_instances_ready_name ON sage.clone_instances USING btree (name, created_at DESC) WHERE (status = 'ready'::text);


--
-- Name: config_derivation_key_recent; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX config_derivation_key_recent ON sage.config_derivation USING btree (key, created_at DESC, id DESC);


--
-- Name: facts_column_class_relid; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX facts_column_class_relid ON sage.facts USING btree (subject_relid) WHERE (fact_type = 'column_class'::text);


--
-- Name: facts_column_class_uniq; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX facts_column_class_uniq ON sage.facts USING btree (subject_relid, subject_attnum, fact_type) WHERE (subject_kind = 'column'::text);


--
-- Name: facts_status_updated; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX facts_status_updated ON sage.facts USING btree (status, updated_at);


--
-- Name: facts_subject_key; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX facts_subject_key ON sage.facts USING btree (fact_type, subject_kind, subject);


--
-- Name: first_look_database_newest; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX first_look_database_newest ON sage.first_look USING btree (database_name, id DESC);


--
-- Name: guard_cluster_roles_broker; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_cluster_roles_broker ON sage.guard_cluster_roles USING btree (broker_role);


--
-- Name: guard_cluster_roles_login; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_cluster_roles_login ON sage.guard_cluster_roles USING btree (login_role);


--
-- Name: guard_cluster_roles_rotated_at; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_cluster_roles_rotated_at ON sage.guard_cluster_roles USING btree (rotated_at) WHERE (status = 'active'::text);


--
-- Name: guard_environment_labels_physical; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_environment_labels_physical ON sage.guard_environment_labels USING btree (((observed ->> 'system_identifier'::text)), ((observed ->> 'db_oid'::text)));


--
-- Name: guard_freezes_kill; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_freezes_kill ON sage.guard_freezes USING btree (kill_id);


--
-- Name: guard_freezes_open; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX guard_freezes_open ON sage.guard_freezes USING btree (scope, target) WHERE (cleared_at IS NULL);


--
-- Name: guard_grant_requests_pending_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_grant_requests_pending_idx ON sage.guard_grant_requests USING btree (principal_id) WHERE (status = 'pending'::text);


--
-- Name: guard_grant_requests_principal_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_grant_requests_principal_idx ON sage.guard_grant_requests USING btree (principal_id, id DESC);


--
-- Name: guard_grants_incomplete_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_grants_incomplete_idx ON sage.guard_grants USING btree (id) WHERE (state = 'revoke_incomplete'::text);


--
-- Name: guard_grants_object_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_grants_object_idx ON sage.guard_grants USING btree (principal_id, object_oid) WHERE (state <> 'revoked'::text);


--
-- Name: guard_grants_open_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_grants_open_idx ON sage.guard_grants USING btree (expires_at) WHERE (revoked_at IS NULL);


--
-- Name: guard_grants_principal_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_grants_principal_idx ON sage.guard_grants USING btree (principal_id, id DESC);


--
-- Name: guard_identity_bindings_principal; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_identity_bindings_principal ON sage.guard_identity_bindings USING btree (principal_id);


--
-- Name: guard_inflight_principal; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_inflight_principal ON sage.guard_inflight USING btree (principal_id);


--
-- Name: guard_pgaudit_events_action; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_pgaudit_events_action ON sage.guard_pgaudit_events USING btree (action_id) WHERE (action_id IS NOT NULL);


--
-- Name: guard_pgaudit_events_logged; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_pgaudit_events_logged ON sage.guard_pgaudit_events USING btree (logged_at);


--
-- Name: guard_principals_retired_at; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_principals_retired_at ON sage.guard_principals USING btree (updated_at) WHERE (status = 'retired'::text);


--
-- Name: guard_principals_retired_by; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_principals_retired_by ON sage.guard_principals USING btree (retired_by) WHERE (retired_by IS NOT NULL);


--
-- Name: guard_principals_sponsor; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_principals_sponsor ON sage.guard_principals USING btree (sponsor_user_id) WHERE (sponsor_user_id IS NOT NULL);


--
-- Name: guard_query_audit_at; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_query_audit_at ON sage.guard_query_audit USING btree (at);


--
-- Name: guard_query_audit_principal_at; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX guard_query_audit_principal_at ON sage.guard_query_audit USING btree (principal_id, at);


--
-- Name: guard_unfreeze_requests_pending; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX guard_unfreeze_requests_pending ON sage.guard_unfreeze_requests USING btree (freeze_id) WHERE (status = 'pending'::text);


--
-- Name: idx_action_log_decision; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_decision ON sage.action_log USING btree (decision_id) WHERE (decision_id IS NOT NULL);


--
-- Name: idx_action_log_drop_index; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_drop_index ON sage.action_log USING btree (executed_at) WHERE (action_type = 'drop_index'::text);


--
-- Name: idx_action_log_finding; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_finding ON sage.action_log USING btree (finding_id) WHERE (finding_id IS NOT NULL);


--
-- Name: idx_action_log_outcome_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_outcome_time ON sage.action_log USING btree (outcome, executed_at DESC);


--
-- Name: idx_action_log_principal; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_principal ON sage.action_log USING btree (principal_id, executed_at DESC) WHERE (principal_id IS NOT NULL);


--
-- Name: idx_action_log_rolled_back; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_rolled_back ON sage.action_log USING btree (measured_at) WHERE (outcome = 'rolled_back'::text);


--
-- Name: idx_action_log_sql_md5; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_sql_md5 ON sage.action_log USING btree (md5(sql_executed), executed_at);


--
-- Name: idx_action_log_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_time ON sage.action_log USING btree (executed_at DESC);


--
-- Name: idx_action_log_value_credit; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_value_credit ON sage.action_log USING btree (executed_at) INCLUDE (action_type, toil_minutes_saved) WHERE ((outcome = 'success'::text) AND (toil_minutes_saved IS NOT NULL));


--
-- Name: idx_action_log_verification; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_log_verification ON sage.action_log USING btree (verification_id) WHERE (verification_id IS NOT NULL);


--
-- Name: idx_action_outcome_class_decided; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_outcome_class_decided ON sage.action_outcome USING btree (action_class, decided_at DESC);


--
-- Name: idx_action_outcome_decided; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_outcome_decided ON sage.action_outcome USING btree (decided_at) WHERE (decided_at IS NOT NULL);


--
-- Name: idx_action_outcome_pending; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_outcome_pending ON sage.action_outcome USING btree (action_log_id) WHERE (verdict = 'pending'::text);


--
-- Name: idx_action_queue_action_log; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_action_log ON sage.action_queue USING btree (action_log_id) WHERE (action_log_id IS NOT NULL);


--
-- Name: idx_action_queue_autonomy; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_autonomy ON sage.action_queue USING btree (action_log_id) WHERE (identity_key ~~ 'autonomy:%'::text);


--
-- Name: idx_action_queue_database_pending; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_database_pending ON sage.action_queue USING btree (database_id, status, proposed_at DESC) WHERE (status = 'pending'::text);


--
-- Name: idx_action_queue_expiry; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_expiry ON sage.action_queue USING btree (expires_at) WHERE (status = ANY (ARRAY['pending'::text, 'failed'::text]));


--
-- Name: idx_action_queue_finding; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_finding ON sage.action_queue USING btree (finding_id) WHERE (status = 'pending'::text);


--
-- Name: idx_action_queue_identity; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_identity ON sage.action_queue USING btree (identity_key, status) WHERE (identity_key IS NOT NULL);


--
-- Name: idx_action_queue_ledger; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_ledger ON sage.action_queue USING btree (proposed_at, id) WHERE ((status <> 'executed'::text) AND (proposed_at IS NOT NULL));


--
-- Name: idx_action_queue_recommendation; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_recommendation ON sage.action_queue USING btree (recommendation_id, status) WHERE (recommendation_id IS NOT NULL);


--
-- Name: idx_action_queue_rejected; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_rejected ON sage.action_queue USING btree (decided_at) WHERE (status = 'rejected'::text);


--
-- Name: idx_action_queue_snoozed; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_snoozed ON sage.action_queue USING btree (snoozed_until) WHERE (snoozed_until IS NOT NULL);


--
-- Name: idx_action_queue_status; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_action_queue_status ON sage.action_queue USING btree (status, proposed_at DESC);


--
-- Name: idx_admission_withheld_recent; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_admission_withheld_recent ON sage.admission_withheld USING btree (database_name, last_seen_at DESC);


--
-- Name: idx_alert_log_dedup; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_alert_log_dedup ON sage.alert_log USING btree (dedup_key, sent_at DESC);


--
-- Name: idx_alert_log_finding; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_alert_log_finding ON sage.alert_log USING btree (finding_id);


--
-- Name: idx_alert_log_sent; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_alert_log_sent ON sage.alert_log USING btree (sent_at);


--
-- Name: idx_approval_card_closed; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_approval_card_closed ON sage.approval_card_deliveries USING btree (followed_up_at) WHERE (followed_up_at IS NOT NULL);


--
-- Name: idx_approval_card_followup; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_approval_card_followup ON sage.approval_card_deliveries USING btree (created_at, id) WHERE (followed_up_at IS NULL);


--
-- Name: idx_auth_audit_created; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_auth_audit_created ON sage.auth_audit USING btree (created_at);


--
-- Name: idx_auth_audit_target; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_auth_audit_target ON sage.auth_audit USING btree (target_user_id, created_at DESC);


--
-- Name: idx_change_lease_database_object_state_unique; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_change_lease_database_object_state_unique ON sage.change_lease USING btree (COALESCE(database_id, (0)::bigint), object_key) WHERE (state = 'active'::text);


--
-- Name: idx_change_lease_decision; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_change_lease_decision ON sage.change_lease USING btree (decision_id);


--
-- Name: idx_change_lease_expiry; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_change_lease_expiry ON sage.change_lease USING btree (expires_at) WHERE (state = 'active'::text);


--
-- Name: idx_chatops_replay_seen; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_chatops_replay_seen ON sage.chatops_replay USING btree (seen_at);


--
-- Name: idx_config_audit_db; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_config_audit_db ON sage.config_audit USING btree (database_id, changed_at DESC);


--
-- Name: idx_config_audit_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_config_audit_time ON sage.config_audit USING btree (changed_at DESC);


--
-- Name: idx_config_key_db; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_config_key_db ON sage.config USING btree (key, COALESCE(database_id, 0));


--
-- Name: idx_decision_action_log; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_action_log ON sage.decision USING btree (action_log_id) WHERE (action_log_id IS NOT NULL);


--
-- Name: idx_decision_autonomy_execute; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_autonomy_execute ON sage.decision USING btree (id) WHERE ((verdict = 'execute'::text) AND (evidence ? 'incident_family'::text));


--
-- Name: idx_decision_created; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_created ON sage.decision USING btree (created_at);


--
-- Name: idx_decision_database_created; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_database_created ON sage.decision USING btree (database_id, created_at DESC);


--
-- Name: idx_decision_evidence_id_unique; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_decision_evidence_id_unique ON sage.decision USING btree (evidence_id);


--
-- Name: idx_decision_fingerprint; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_decision_fingerprint ON sage.decision USING btree (fingerprint) WHERE ((fingerprint IS NOT NULL) AND (resolved_at IS NULL));


--
-- Name: idx_decision_open_deadline; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_open_deadline ON sage.decision USING btree (deadline_hard_at) WHERE ((resolved_at IS NULL) AND (deadline_kind IS NOT NULL));


--
-- Name: idx_decision_policy; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_policy ON sage.decision USING btree (policy_id) WHERE (policy_id IS NOT NULL);


--
-- Name: idx_decision_queue; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_queue ON sage.decision USING btree (queue_id) WHERE (queue_id IS NOT NULL);


--
-- Name: idx_decision_schema_guard_counted; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_schema_guard_counted ON sage.decision USING gin (target_objects) WHERE ((feature = 'schema_guard'::text) AND (((evidence ->> 'disposition'::text) = 'dry_run'::text) OR ((evidence ->> 'external_reversion'::text) = 'true'::text)));


--
-- Name: idx_decision_schema_guard_key; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_decision_schema_guard_key ON sage.decision USING btree (((evidence ->> 'invariant_key'::text)), id DESC) WHERE (feature = 'schema_guard'::text);


--
-- Name: idx_explain_cache_captured; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_explain_cache_captured ON sage.explain_cache USING btree (captured_at);


--
-- Name: idx_explain_provider_event; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_explain_provider_event ON sage.explain_cache USING btree (provider_scope_hash, provider_event_id) WHERE (provider_event_id IS NOT NULL);


--
-- Name: idx_explain_queryid; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_explain_queryid ON sage.explain_cache USING btree (queryid, captured_at DESC);


--
-- Name: idx_explain_results_expires; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_explain_results_expires ON sage.explain_results USING btree (expires_at);


--
-- Name: idx_explain_source; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_explain_source ON sage.explain_cache USING btree (source, queryid, captured_at DESC);


--
-- Name: idx_fact_card_created; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_fact_card_created ON sage.fact_card_deliveries USING btree (created_at);


--
-- Name: idx_findings_action_log; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_action_log ON sage.findings USING btree (action_log_id) WHERE (action_log_id IS NOT NULL);


--
-- Name: idx_findings_category_status; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_category_status ON sage.findings USING btree (category, severity) WHERE (status = 'open'::text);


--
-- Name: idx_findings_dedup; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_findings_dedup ON sage.findings USING btree (category, object_identifier) WHERE (status = 'open'::text);


--
-- Name: idx_findings_list_rank; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_list_rank ON sage.findings USING btree (status, (
CASE severity
    WHEN 'critical'::text THEN 3
    WHEN 'warning'::text THEN 2
    WHEN 'info'::text THEN 1
    ELSE 0
END), id);


--
-- Name: idx_findings_migration_feed; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_migration_feed ON sage.findings USING btree (id) WHERE (category = 'migration_safety'::text);


--
-- Name: idx_findings_object; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_object ON sage.findings USING btree (object_identifier, category);


--
-- Name: idx_findings_resolved_at; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_resolved_at ON sage.findings USING btree (resolved_at) WHERE (status = 'resolved'::text);


--
-- Name: idx_findings_rule_id; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_rule_id ON sage.findings USING btree (rule_id) WHERE (rule_id IS NOT NULL);


--
-- Name: idx_findings_schema_lint_open; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_schema_lint_open ON sage.findings USING btree (category, severity) WHERE ((category ~~ 'schema_lint:%'::text) AND (status = 'open'::text));


--
-- Name: idx_findings_status_severity; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_findings_status_severity ON sage.findings USING btree (status, severity);


--
-- Name: idx_health_history_lookup; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_health_history_lookup ON sage.health_history USING btree (database_name, recorded_at DESC);


--
-- Name: idx_health_history_recent; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_health_history_recent ON sage.health_history USING btree (recorded_at DESC);


--
-- Name: idx_incident_avoided_action_log; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incident_avoided_action_log ON sage.incident_avoided USING btree (action_log_id);


--
-- Name: idx_incident_avoided_database_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incident_avoided_database_time ON sage.incident_avoided USING btree (database_id, occurred_at DESC);


--
-- Name: idx_incident_avoided_decision; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incident_avoided_decision ON sage.incident_avoided USING btree (decision_id);


--
-- Name: idx_incident_avoided_evidence_unique; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_incident_avoided_evidence_unique ON sage.incident_avoided USING btree (evidence_id);


--
-- Name: idx_incident_avoided_verify; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incident_avoided_verify ON sage.incident_avoided USING btree (verification_id);


--
-- Name: idx_incidents_active; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incidents_active ON sage.incidents USING btree (detected_at DESC) WHERE (resolved_at IS NULL);


--
-- Name: idx_incidents_autovacuum_cancel; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incidents_autovacuum_cancel ON sage.incidents USING btree (detected_at) WHERE ('log_autovacuum_cancel'::text = ANY (signal_ids));


--
-- Name: idx_incidents_database; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incidents_database ON sage.incidents USING btree (database_name, detected_at DESC);


--
-- Name: idx_incidents_identity_open; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incidents_identity_open ON sage.incidents USING btree (database_name, identity_key) WHERE (resolved_at IS NULL);


--
-- Name: idx_incidents_identity_resolved; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incidents_identity_resolved ON sage.incidents USING btree (identity_key, resolved_at DESC) WHERE (resolved_at IS NOT NULL);


--
-- Name: idx_incidents_previous; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incidents_previous ON sage.incidents USING btree (previous_incident_id) WHERE (previous_incident_id IS NOT NULL);


--
-- Name: idx_incidents_resolved_at; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incidents_resolved_at ON sage.incidents USING btree (resolved_at) WHERE (resolved_at IS NOT NULL);


--
-- Name: idx_incidents_severity; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_incidents_severity ON sage.incidents USING btree (severity) WHERE (resolved_at IS NULL);


--
-- Name: idx_index_replace_action; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_index_replace_action ON sage.index_replace USING btree (action_log_id) WHERE (action_log_id IS NOT NULL);


--
-- Name: idx_index_replace_open; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_index_replace_open ON sage.index_replace USING btree (id) WHERE ((state = ANY (ARRAY['creating'::text, 'created'::text, 'dropping'::text, 'rollback_recreating'::text, 'rollback_dropping'::text, 'old_restoring'::text])) OR (verify_phase = ANY (ARRAY['judging'::text, 'watch'::text])));


--
-- Name: idx_io_rate_sample_database_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_io_rate_sample_database_time ON sage.io_rate_sample USING btree (database_name, sampled_at);


--
-- Name: idx_lease_queue_waiting_keys; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_lease_queue_waiting_keys ON sage.lease_queue USING gin (object_keys) WHERE (state = 'waiting'::text);


--
-- Name: idx_lease_queue_waiting_request; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_lease_queue_waiting_request ON sage.lease_queue USING btree (COALESCE(database_id, (0)::bigint), request_key) WHERE (state = 'waiting'::text);


--
-- Name: idx_notification_log_channel_sent; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_notification_log_channel_sent ON sage.notification_log USING btree (channel_id, sent_at DESC) WHERE (channel_id IS NOT NULL);


--
-- Name: idx_notification_log_sent_at; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_notification_log_sent_at ON sage.notification_log USING btree (sent_at DESC);


--
-- Name: idx_notification_rules_event_enabled; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_notification_rules_event_enabled ON sage.notification_rules USING btree (event, min_severity) WHERE (enabled = true);


--
-- Name: idx_optimizer_rejection_measured; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_optimizer_rejection_measured ON sage.optimizer_rejection USING btree (measured_at);


--
-- Name: idx_policy_active_database_status; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_policy_active_database_status ON sage.policy USING btree (COALESCE(database_id, (0)::bigint), scope) WHERE (status = 'active'::text);


--
-- Name: idx_policy_approvals_review; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_policy_approvals_review ON sage.policy_approvals USING btree (decided_at) WHERE (review_status = 'pending'::text);


--
-- Name: idx_policy_history; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_policy_history ON sage.policy USING btree (database_id, version DESC);


--
-- Name: idx_query_hints_queryid; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_query_hints_queryid ON sage.query_hints USING btree (queryid) WHERE (status = 'active'::text);


--
-- Name: idx_query_hints_revalidate; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_query_hints_revalidate ON sage.query_hints USING btree (last_revalidated_at NULLS FIRST) WHERE (status = 'active'::text);


--
-- Name: idx_query_store_qid_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_query_store_qid_time ON ONLY sage.query_store USING btree (queryid, captured_at DESC);


--
-- Name: idx_query_store_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_query_store_time ON ONLY sage.query_store USING btree (captured_at DESC);


--
-- Name: idx_retention_run_dry_run; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_retention_run_dry_run ON sage.retention_run USING btree (dry_run_id) WHERE (dry_run_id IS NOT NULL);


--
-- Name: idx_retention_run_target; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_retention_run_target ON sage.retention_run USING btree (schema_name, table_name, created_at DESC);


--
-- Name: idx_schema_baseline_action; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_schema_baseline_action ON sage.schema_baseline USING btree (last_authorized_action_id) WHERE (last_authorized_action_id IS NOT NULL);


--
-- Name: idx_schema_baseline_decision; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_schema_baseline_decision ON sage.schema_baseline USING btree (last_authorized_decision_id) WHERE (last_authorized_decision_id IS NOT NULL);


--
-- Name: idx_schema_baseline_reconciliation; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_schema_baseline_reconciliation ON sage.schema_baseline USING btree (database_id, reconciliation_status);


--
-- Name: idx_schema_findings_active_key; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_schema_findings_active_key ON sage.schema_findings USING btree (rule_id, schema_name, table_name, column_name, index_name) WHERE (resolved_at IS NULL);


--
-- Name: idx_schema_findings_severity; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_schema_findings_severity ON sage.schema_findings USING btree (severity) WHERE (resolved_at IS NULL);


--
-- Name: idx_schema_findings_table; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_schema_findings_table ON sage.schema_findings USING btree (schema_name, table_name);


--
-- Name: idx_sessions_expires; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sessions_expires ON sage.sessions USING btree (expires_at);


--
-- Name: idx_sessions_user; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sessions_user ON sage.sessions USING btree (user_id);


--
-- Name: idx_shadow_decision_class_summary; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_shadow_decision_class_summary ON sage.shadow_decision USING btree (family, action_class) INCLUDE (status, score, counted, recorded_at);


--
-- Name: idx_size_history_lookup; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_size_history_lookup ON sage.size_history USING btree (metric_type, object_name, collected_at DESC);


--
-- Name: idx_size_history_recent; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_size_history_recent ON sage.size_history USING btree (collected_at DESC);


--
-- Name: idx_snapshots_base; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_snapshots_base ON ONLY sage.snapshots USING btree (base_id) WHERE (base_id IS NOT NULL);


--
-- Name: idx_snapshots_category; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_snapshots_category ON ONLY sage.snapshots USING btree (category, collected_at DESC);


--
-- Name: idx_snapshots_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_snapshots_time ON ONLY sage.snapshots USING btree (collected_at DESC);


--
-- Name: idx_sre_action_proposals_one_executing; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_sre_action_proposals_one_executing ON sage.sre_action_proposals USING btree (deployment_id, database_id) WHERE (state = 'executing'::text);


--
-- Name: idx_sre_action_proposals_queue; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_action_proposals_queue ON sage.sre_action_proposals USING btree (queue_id) WHERE (queue_id IS NOT NULL);


--
-- Name: idx_sre_action_proposals_recovery; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_action_proposals_recovery ON sage.sre_action_proposals USING btree (deployment_id, database_id, next_sample_at) WHERE (recovery_state = 'observing'::text);


--
-- Name: idx_sre_action_proposals_state; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_action_proposals_state ON sage.sre_action_proposals USING btree (deployment_id, database_id, state, updated_at);


--
-- Name: idx_sre_autonomy_outcomes_harmful; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_autonomy_outcomes_harmful ON sage.sre_autonomy_outcomes USING btree (deployment_id, database_name, family, recorded_at DESC) WHERE (result = ANY (ARRAY['harmful'::text, 'safety_violation'::text]));


--
-- Name: idx_sre_change_events_received; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_change_events_received ON sage.sre_change_events USING btree (deployment_id, received_at);


--
-- Name: idx_sre_investigations_case; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_investigations_case ON sage.sre_investigations USING btree (deployment_id, database_id, source_case_id, created_at DESC, id DESC);


--
-- Name: idx_sre_investigations_created; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_investigations_created ON sage.sre_investigations USING btree (deployment_id, database_id, created_at DESC, id DESC);


--
-- Name: idx_sre_investigations_queue; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_investigations_queue ON sage.sre_investigations USING btree (deployment_id, database_id, state, created_at);


--
-- Name: idx_sre_maintenance_proposals_finding; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_maintenance_proposals_finding ON sage.sre_maintenance_proposals USING btree (((proposal ->> 'finding_id'::text)));


--
-- Name: idx_sre_maintenance_proposals_identity; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_sre_maintenance_proposals_identity ON sage.sre_maintenance_proposals USING btree ((('sre_maintenance:'::text || (id)::text)));


--
-- Name: idx_table_contract_identity; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_table_contract_identity ON sage.table_contract USING btree (COALESCE(database_id, (0)::bigint), schema_name, table_name);


--
-- Name: idx_toil_model_action_type_model_version_unique; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_toil_model_action_type_model_version_unique ON sage.toil_model USING btree (action_type, model_version);


--
-- Name: idx_toil_model_active_action; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_toil_model_active_action ON sage.toil_model USING btree (action_type) WHERE (effective_to IS NULL);


--
-- Name: idx_tuning_budget_day_updated; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_tuning_budget_day_updated ON sage.tuning_budget_day USING btree (updated_at);


--
-- Name: idx_user_oidc_link_grants_user; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_user_oidc_link_grants_user ON sage.user_oidc_link_grants USING btree (user_id) WHERE (used_at IS NULL);


--
-- Name: idx_users_oauth_identity; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX idx_users_oauth_identity ON sage.users USING btree (oauth_issuer, oauth_subject) WHERE (oauth_issuer IS NOT NULL);


--
-- Name: idx_verification_action; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_verification_action ON sage.verification USING btree (action_log_id) WHERE (action_log_id IS NOT NULL);


--
-- Name: idx_verification_created; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_verification_created ON sage.verification USING btree (created_at);


--
-- Name: idx_verification_decision; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_verification_decision ON sage.verification USING btree (decision_id);


--
-- Name: idx_verification_due; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_verification_due ON sage.verification USING btree (next_evaluation_at) WHERE (verdict = ANY (ARRAY['pending'::text, 'extended'::text]));


--
-- Name: idx_verification_open_due; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_verification_open_due ON sage.verification USING btree (next_evaluation_at) WHERE (completed_at IS NULL);


--
-- Name: idx_verification_watch; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX idx_verification_watch ON sage.verification USING btree (((baseline ->> 'watch_id'::text)));


--
-- Name: managed_change_open_fingerprint_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX managed_change_open_fingerprint_idx ON sage.managed_change_proposals USING btree (fingerprint) WHERE (status = ANY (ARRAY['pending'::text, 'approved'::text]));


--
-- Name: managed_change_parameter_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX managed_change_parameter_idx ON sage.managed_change_proposals USING btree (provider, target, parameter) WHERE (status = ANY (ARRAY['pending'::text, 'approved'::text]));


--
-- Name: managed_change_status_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX managed_change_status_idx ON sage.managed_change_proposals USING btree (status, created_at DESC);


--
-- Name: mcp_tokens_created; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX mcp_tokens_created ON sage.mcp_tokens USING btree (created_at DESC);


--
-- Name: mcp_tokens_owner; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX mcp_tokens_owner ON sage.mcp_tokens USING btree (owner_user_id);


--
-- Name: mcp_tokens_principal; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX mcp_tokens_principal ON sage.mcp_tokens USING btree (principal_id) WHERE (principal_id IS NOT NULL);


--
-- Name: query_store_default_captured_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX query_store_default_captured_at_idx ON sage.query_store_default USING btree (captured_at DESC);


--
-- Name: query_store_default_queryid_captured_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX query_store_default_queryid_captured_at_idx ON sage.query_store_default USING btree (queryid, captured_at DESC);


--
-- Name: query_store_history_i1; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX query_store_history_i1 ON sage.query_store_history USING btree (queryid, captured_at DESC);


--
-- Name: query_store_history_i2; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX query_store_history_i2 ON sage.query_store_history USING btree (captured_at DESC);


--
-- Name: query_store_p20261117_captured_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX query_store_p20261117_captured_at_idx ON sage.query_store_p20261117 USING btree (captured_at DESC);


--
-- Name: query_store_p20261117_queryid_captured_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX query_store_p20261117_queryid_captured_at_idx ON sage.query_store_p20261117 USING btree (queryid, captured_at DESC);


--
-- Name: query_store_p20261118_captured_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX query_store_p20261118_captured_at_idx ON sage.query_store_p20261118 USING btree (captured_at DESC);


--
-- Name: query_store_p20261118_queryid_captured_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX query_store_p20261118_queryid_captured_at_idx ON sage.query_store_p20261118 USING btree (queryid, captured_at DESC);


--
-- Name: recommendation_action; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX recommendation_action ON sage.recommendation USING btree (action_log_id) WHERE (action_log_id IS NOT NULL);


--
-- Name: recommendation_actionable; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX recommendation_actionable ON sage.recommendation USING btree (database_name, state, next_attempt_at);


--
-- Name: recommendation_finding; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX recommendation_finding ON sage.recommendation USING btree (finding_id) WHERE (finding_id IS NOT NULL);


--
-- Name: recommendation_identity; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX recommendation_identity ON sage.recommendation USING btree (identity_key, id DESC);


--
-- Name: recommendation_one_live; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX recommendation_one_live ON sage.recommendation USING btree (identity_key) WHERE (state = ANY (ARRAY['proposed'::text, 'approved'::text, 'applying'::text, 'applied'::text, 'verifying'::text, 'failed'::text]));


--
-- Name: recommendation_terminal_age; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX recommendation_terminal_age ON sage.recommendation USING btree (updated_at) WHERE (state = ANY (ARRAY['verified'::text, 'reverted'::text, 'inconclusive'::text, 'superseded'::text, 'abandoned'::text]));


--
-- Name: recommendation_transition_history; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX recommendation_transition_history ON sage.recommendation_transition USING btree (recommendation_id, id);


--
-- Name: runway_samples_sampled_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX runway_samples_sampled_at_idx ON sage.runway_samples USING btree (sampled_at);


--
-- Name: runway_samples_series_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX runway_samples_series_idx ON sage.runway_samples USING btree (kind, subject, sampled_at DESC);


--
-- Name: shadow_decision_class_recent; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX shadow_decision_class_recent ON sage.shadow_decision USING btree (action_class, recorded_at DESC);


--
-- Name: shadow_decision_fingerprint_recent; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX shadow_decision_fingerprint_recent ON sage.shadow_decision USING btree (fingerprint, recorded_at DESC);


--
-- Name: shadow_decision_one_pending; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX shadow_decision_one_pending ON sage.shadow_decision USING btree (COALESCE(database_id, 0), fingerprint) WHERE (status = 'pending'::text);


--
-- Name: shadow_decision_pending; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX shadow_decision_pending ON sage.shadow_decision USING btree (recorded_at) WHERE (status = 'pending'::text);


--
-- Name: shadow_decision_scored; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX shadow_decision_scored ON sage.shadow_decision USING btree (scored_at) WHERE ((status = 'scored'::text) AND counted);


--
-- Name: snapshots_default_base_id_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_default_base_id_idx ON sage.snapshots_default USING btree (base_id) WHERE (base_id IS NOT NULL);


--
-- Name: snapshots_default_category_collected_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_default_category_collected_at_idx ON sage.snapshots_default USING btree (category, collected_at DESC);


--
-- Name: snapshots_default_collected_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_default_collected_at_idx ON sage.snapshots_default USING btree (collected_at DESC);


--
-- Name: snapshots_history_i1; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_history_i1 ON sage.snapshots_history USING btree (base_id) WHERE (base_id IS NOT NULL);


--
-- Name: snapshots_history_i2; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_history_i2 ON sage.snapshots_history USING btree (category, collected_at DESC);


--
-- Name: snapshots_history_i3; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_history_i3 ON sage.snapshots_history USING btree (collected_at DESC);


--
-- Name: snapshots_p20261117_base_id_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_p20261117_base_id_idx ON sage.snapshots_p20261117 USING btree (base_id) WHERE (base_id IS NOT NULL);


--
-- Name: snapshots_p20261117_category_collected_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_p20261117_category_collected_at_idx ON sage.snapshots_p20261117 USING btree (category, collected_at DESC);


--
-- Name: snapshots_p20261117_collected_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_p20261117_collected_at_idx ON sage.snapshots_p20261117 USING btree (collected_at DESC);


--
-- Name: snapshots_p20261118_base_id_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_p20261118_base_id_idx ON sage.snapshots_p20261118 USING btree (base_id) WHERE (base_id IS NOT NULL);


--
-- Name: snapshots_p20261118_category_collected_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_p20261118_category_collected_at_idx ON sage.snapshots_p20261118 USING btree (category, collected_at DESC);


--
-- Name: snapshots_p20261118_collected_at_idx; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX snapshots_p20261118_collected_at_idx ON sage.snapshots_p20261118 USING btree (collected_at DESC);


--
-- Name: source_fix_finding; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX source_fix_finding ON sage.source_fix USING btree (finding_id);


--
-- Name: specialist_requests_created; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX specialist_requests_created ON sage.specialist_requests USING btree (created_at DESC);


--
-- Name: specialist_requests_external; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX specialist_requests_external ON sage.specialist_requests USING btree (((external_ref ->> 'system'::text)), ((external_ref ->> 'id'::text))) WHERE (outbound <> 'none'::text);


--
-- Name: specialist_requests_investigation; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX specialist_requests_investigation ON sage.specialist_requests USING btree (investigation_id, token_id);


--
-- Name: specialist_requests_outbound; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX specialist_requests_outbound ON sage.specialist_requests USING btree (outbound_next_at) WHERE (outbound = 'pending'::text);


--
-- Name: specialist_requests_token_live; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX specialist_requests_token_live ON sage.specialist_requests USING btree (token_id, created_at DESC) WHERE ((kind = 'open'::text) AND created AND (terminal_at IS NULL));


--
-- Name: sre_autonomy_events_db_pair; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_autonomy_events_db_pair ON sage.sre_autonomy_events USING btree (deployment_id, database_name, family, action_class, created_at DESC);


--
-- Name: sre_autonomy_events_pair; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_autonomy_events_pair ON sage.sre_autonomy_events USING btree (deployment_id, family, action_class, created_at DESC);


--
-- Name: sre_autonomy_one_auto_executed; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX sre_autonomy_one_auto_executed ON sage.sre_autonomy_events USING btree (deployment_id, database_name, action_log_id) WHERE (event_type = 'auto_executed'::text);


--
-- Name: sre_autonomy_one_pending; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX sre_autonomy_one_pending ON sage.sre_autonomy_proposals USING btree (deployment_id, database_name, family, action_class) WHERE (status = 'pending'::text);


--
-- Name: sre_autonomy_outcome_per_action; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX sre_autonomy_outcome_per_action ON sage.sre_autonomy_outcomes USING btree (deployment_id, database_name, action_log_id, source) WHERE (action_log_id IS NOT NULL);


--
-- Name: sre_autonomy_outcome_per_queue; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX sre_autonomy_outcome_per_queue ON sage.sre_autonomy_outcomes USING btree (deployment_id, database_name, queue_id) WHERE (queue_id IS NOT NULL);


--
-- Name: sre_autonomy_outcomes_db_family; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_autonomy_outcomes_db_family ON sage.sre_autonomy_outcomes USING btree (deployment_id, database_name, family, recorded_at DESC);


--
-- Name: sre_autonomy_outcomes_db_pair; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_autonomy_outcomes_db_pair ON sage.sre_autonomy_outcomes USING btree (deployment_id, database_name, family, action_class);


--
-- Name: sre_autonomy_outcomes_family; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_autonomy_outcomes_family ON sage.sre_autonomy_outcomes USING btree (deployment_id, family, recorded_at DESC);


--
-- Name: sre_budget_day; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_budget_day ON sage.sre_budget_reservations USING btree (deployment_id, utc_day, database_id);


--
-- Name: sre_budget_request; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX sre_budget_request ON sage.sre_budget_reservations USING btree (deployment_id, database_id, COALESCE(investigation_id, '00000000-0000-0000-0000-000000000000'::uuid), request_key);


--
-- Name: sre_change_events_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_change_events_time ON sage.sre_change_events USING btree (deployment_id, occurred_at DESC);


--
-- Name: sre_eval_runs_latest; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_eval_runs_latest ON sage.sre_eval_runs USING btree (deployment_id, source, generated_at DESC);


--
-- Name: sre_evidence_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_evidence_time ON sage.sre_evidence USING btree (deployment_id, database_id, investigation_id, observed_at);


--
-- Name: sre_game_days_latest; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_game_days_latest ON sage.sre_game_days USING btree (deployment_id, database_name, started_at DESC);


--
-- Name: sre_investigation_idempotency; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX sre_investigation_idempotency ON sage.sre_investigations USING btree (deployment_id, database_id, idempotency_key) WHERE (idempotency_key IS NOT NULL);


--
-- Name: sre_investigation_memory; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_investigation_memory ON sage.sre_investigations USING btree (deployment_id, database_id, trigger_kind, concluded_at) WHERE (state = ANY (ARRAY['concluded'::text, 'inconclusive'::text]));


--
-- Name: sre_one_live_trigger; Type: INDEX; Schema: sage; Owner: -
--

CREATE UNIQUE INDEX sre_one_live_trigger ON sage.sre_investigations USING btree (deployment_id, database_id, trigger_fingerprint) WHERE (state = ANY (ARRAY['queued'::text, 'collecting'::text, 'evaluating'::text, 'needs_evidence'::text, 'paused'::text]));


--
-- Name: sre_outcomes_by_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_outcomes_by_time ON sage.sre_investigation_outcomes USING btree (deployment_id, database_id, investigation_id, recorded_at DESC);


--
-- Name: sre_packet_reviews_db_family; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_packet_reviews_db_family ON sage.sre_packet_reviews USING btree (deployment_id, database_name, family, reviewed_at);


--
-- Name: sre_packet_reviews_family; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_packet_reviews_family ON sage.sre_packet_reviews USING btree (deployment_id, family, reviewed_at);


--
-- Name: sre_runbook_runs_by_runbook; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_runbook_runs_by_runbook ON sage.sre_runbook_runs USING btree (deployment_id, database_id, runbook_id, created_at DESC);


--
-- Name: sre_sli_samples_time; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_sli_samples_time ON sage.sre_sli_samples USING btree (deployment_id, observed_at);


--
-- Name: sre_slo_transitions_by_slo; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX sre_slo_transitions_by_slo ON sage.sre_slo_transitions USING btree (deployment_id, database_id, name, at DESC);


--
-- Name: trust_shadow_evidence_pair; Type: INDEX; Schema: sage; Owner: -
--

CREATE INDEX trust_shadow_evidence_pair ON sage.trust_shadow_evidence USING btree (deployment_id, database_name, family, action_class);


--
-- Name: query_store_default_captured_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_query_store_time ATTACH PARTITION sage.query_store_default_captured_at_idx;


--
-- Name: query_store_default_queryid_captured_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_query_store_qid_time ATTACH PARTITION sage.query_store_default_queryid_captured_at_idx;


--
-- Name: query_store_history_i1; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_query_store_qid_time ATTACH PARTITION sage.query_store_history_i1;


--
-- Name: query_store_history_i2; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_query_store_time ATTACH PARTITION sage.query_store_history_i2;


--
-- Name: query_store_p20261117_captured_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_query_store_time ATTACH PARTITION sage.query_store_p20261117_captured_at_idx;


--
-- Name: query_store_p20261117_queryid_captured_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_query_store_qid_time ATTACH PARTITION sage.query_store_p20261117_queryid_captured_at_idx;


--
-- Name: query_store_p20261118_captured_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_query_store_time ATTACH PARTITION sage.query_store_p20261118_captured_at_idx;


--
-- Name: query_store_p20261118_queryid_captured_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_query_store_qid_time ATTACH PARTITION sage.query_store_p20261118_queryid_captured_at_idx;


--
-- Name: snapshots_default_base_id_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_base ATTACH PARTITION sage.snapshots_default_base_id_idx;


--
-- Name: snapshots_default_category_collected_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_category ATTACH PARTITION sage.snapshots_default_category_collected_at_idx;


--
-- Name: snapshots_default_collected_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_time ATTACH PARTITION sage.snapshots_default_collected_at_idx;


--
-- Name: snapshots_default_pkey; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.snapshots_pkey ATTACH PARTITION sage.snapshots_default_pkey;


--
-- Name: snapshots_history_i1; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_base ATTACH PARTITION sage.snapshots_history_i1;


--
-- Name: snapshots_history_i2; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_category ATTACH PARTITION sage.snapshots_history_i2;


--
-- Name: snapshots_history_i3; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_time ATTACH PARTITION sage.snapshots_history_i3;


--
-- Name: snapshots_history_pkey; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.snapshots_pkey ATTACH PARTITION sage.snapshots_history_pkey;


--
-- Name: snapshots_p20261117_base_id_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_base ATTACH PARTITION sage.snapshots_p20261117_base_id_idx;


--
-- Name: snapshots_p20261117_category_collected_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_category ATTACH PARTITION sage.snapshots_p20261117_category_collected_at_idx;


--
-- Name: snapshots_p20261117_collected_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_time ATTACH PARTITION sage.snapshots_p20261117_collected_at_idx;


--
-- Name: snapshots_p20261117_pkey; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.snapshots_pkey ATTACH PARTITION sage.snapshots_p20261117_pkey;


--
-- Name: snapshots_p20261118_base_id_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_base ATTACH PARTITION sage.snapshots_p20261118_base_id_idx;


--
-- Name: snapshots_p20261118_category_collected_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_category ATTACH PARTITION sage.snapshots_p20261118_category_collected_at_idx;


--
-- Name: snapshots_p20261118_collected_at_idx; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.idx_snapshots_time ATTACH PARTITION sage.snapshots_p20261118_collected_at_idx;


--
-- Name: snapshots_p20261118_pkey; Type: INDEX ATTACH; Schema: sage; Owner: -
--

ALTER INDEX sage.snapshots_pkey ATTACH PARTITION sage.snapshots_p20261118_pkey;


--
-- Name: action_log audit_chain_v1_id; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE CONSTRAINT TRIGGER audit_chain_v1_id AFTER INSERT OR DELETE ON sage.action_log DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION sage.audit_chain_link_action_log();


--
-- Name: auth_audit audit_chain_v1_id; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE CONSTRAINT TRIGGER audit_chain_v1_id AFTER INSERT OR DELETE ON sage.auth_audit DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION sage.audit_chain_link_auth_audit();


--
-- Name: config_audit audit_chain_v1_id; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE CONSTRAINT TRIGGER audit_chain_v1_id AFTER INSERT OR DELETE ON sage.config_audit DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION sage.audit_chain_link_config_audit();


--
-- Name: guard_pgaudit_events audit_chain_v1_id; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE CONSTRAINT TRIGGER audit_chain_v1_id AFTER INSERT OR DELETE ON sage.guard_pgaudit_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION sage.audit_chain_link_pgaudit_events();


--
-- Name: guard_query_audit audit_chain_v1_id; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE CONSTRAINT TRIGGER audit_chain_v1_id AFTER INSERT OR DELETE ON sage.guard_query_audit DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION sage.audit_chain_link_guard_query_audit();


--
-- Name: action_log audit_chain_v1_t; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER audit_chain_v1_t BEFORE TRUNCATE ON sage.action_log FOR EACH STATEMENT EXECUTE FUNCTION sage.audit_chain_truncate('action_log');


--
-- Name: auth_audit audit_chain_v1_t; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER audit_chain_v1_t BEFORE TRUNCATE ON sage.auth_audit FOR EACH STATEMENT EXECUTE FUNCTION sage.audit_chain_truncate('auth_audit');


--
-- Name: config_audit audit_chain_v1_t; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER audit_chain_v1_t BEFORE TRUNCATE ON sage.config_audit FOR EACH STATEMENT EXECUTE FUNCTION sage.audit_chain_truncate('config_audit');


--
-- Name: guard_pgaudit_events audit_chain_v1_t; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER audit_chain_v1_t BEFORE TRUNCATE ON sage.guard_pgaudit_events FOR EACH STATEMENT EXECUTE FUNCTION sage.audit_chain_truncate('pgaudit_events');


--
-- Name: guard_query_audit audit_chain_v1_t; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER audit_chain_v1_t BEFORE TRUNCATE ON sage.guard_query_audit FOR EACH STATEMENT EXECUTE FUNCTION sage.audit_chain_truncate('guard_query_audit');


--
-- Name: action_log audit_chain_v1_u; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE CONSTRAINT TRIGGER audit_chain_v1_u AFTER UPDATE ON sage.action_log DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (((jsonb_build_array(old.outcome, old.rollback_reason, ((EXTRACT(epoch FROM old.measured_at) * (1000000)::numeric))::bigint, old.after_state, old.verification_id, old.justification, old.toil_minutes_saved, old.toil_model_version))::text IS DISTINCT FROM (jsonb_build_array(new.outcome, new.rollback_reason, ((EXTRACT(epoch FROM new.measured_at) * (1000000)::numeric))::bigint, new.after_state, new.verification_id, new.justification, new.toil_minutes_saved, new.toil_model_version))::text)) EXECUTE FUNCTION sage.audit_chain_link_action_log();


--
-- Name: recommendation_revision recommendation_revision_immutable; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER recommendation_revision_immutable BEFORE UPDATE ON sage.recommendation_revision FOR EACH ROW EXECUTE FUNCTION sage.recommendation_immutable();


--
-- Name: recommendation_transition recommendation_transition_immutable; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER recommendation_transition_immutable BEFORE UPDATE ON sage.recommendation_transition FOR EACH ROW EXECUTE FUNCTION sage.recommendation_immutable();


--
-- Name: sre_autonomy_events sre_autonomy_events_append_only; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER sre_autonomy_events_append_only BEFORE UPDATE ON sage.sre_autonomy_events FOR EACH ROW EXECUTE FUNCTION sage.sre_append_only();


--
-- Name: sre_autonomy_outcomes sre_autonomy_outcomes_append_only; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER sre_autonomy_outcomes_append_only BEFORE UPDATE ON sage.sre_autonomy_outcomes FOR EACH ROW EXECUTE FUNCTION sage.sre_append_only();


--
-- Name: sre_events sre_events_append_only; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER sre_events_append_only BEFORE UPDATE ON sage.sre_events FOR EACH ROW EXECUTE FUNCTION sage.sre_append_only();


--
-- Name: sre_evidence sre_evidence_append_only; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER sre_evidence_append_only BEFORE UPDATE ON sage.sre_evidence FOR EACH ROW EXECUTE FUNCTION sage.sre_append_only();


--
-- Name: sre_runbook_versions sre_runbook_versions_guard; Type: TRIGGER; Schema: sage; Owner: -
--

CREATE TRIGGER sre_runbook_versions_guard BEFORE DELETE OR UPDATE ON sage.sre_runbook_versions FOR EACH ROW EXECUTE FUNCTION sage.sre_runbook_version_guard();


--
-- Name: action_log action_log_decision_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_log
    ADD CONSTRAINT action_log_decision_id_fkey FOREIGN KEY (decision_id) REFERENCES sage.decision(id) ON DELETE SET NULL;


--
-- Name: action_log action_log_verification_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_log
    ADD CONSTRAINT action_log_verification_id_fkey FOREIGN KEY (verification_id) REFERENCES sage.verification(id) ON DELETE SET NULL;


--
-- Name: action_outcome action_outcome_action_log_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_outcome
    ADD CONSTRAINT action_outcome_action_log_id_fkey FOREIGN KEY (action_log_id) REFERENCES sage.action_log(id) ON DELETE CASCADE;


--
-- Name: action_queue action_queue_action_log_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.action_queue
    ADD CONSTRAINT action_queue_action_log_id_fkey FOREIGN KEY (action_log_id) REFERENCES sage.action_log(id) ON DELETE SET NULL;


--
-- Name: alert_log alert_log_finding_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.alert_log
    ADD CONSTRAINT alert_log_finding_id_fkey FOREIGN KEY (finding_id) REFERENCES sage.findings(id) ON DELETE SET NULL;


--
-- Name: ask_messages ask_messages_conversation_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.ask_messages
    ADD CONSTRAINT ask_messages_conversation_id_fkey FOREIGN KEY (conversation_id) REFERENCES sage.ask_conversations(id) ON DELETE CASCADE;


--
-- Name: change_lease change_lease_decision_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.change_lease
    ADD CONSTRAINT change_lease_decision_id_fkey FOREIGN KEY (decision_id) REFERENCES sage.decision(id);


--
-- Name: chatops_identities chatops_identities_user_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.chatops_identities
    ADD CONSTRAINT chatops_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES sage.users(id) ON DELETE CASCADE;


--
-- Name: config_audit config_audit_changed_by_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.config_audit
    ADD CONSTRAINT config_audit_changed_by_fkey FOREIGN KEY (changed_by) REFERENCES sage.users(id);


--
-- Name: config config_updated_by_user_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.config
    ADD CONSTRAINT config_updated_by_user_id_fkey FOREIGN KEY (updated_by_user_id) REFERENCES sage.users(id);


--
-- Name: decision decision_action_log_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.decision
    ADD CONSTRAINT decision_action_log_id_fkey FOREIGN KEY (action_log_id) REFERENCES sage.action_log(id) ON DELETE SET NULL;


--
-- Name: decision decision_policy_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.decision
    ADD CONSTRAINT decision_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES sage.policy(id);


--
-- Name: decision decision_queue_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.decision
    ADD CONSTRAINT decision_queue_id_fkey FOREIGN KEY (queue_id) REFERENCES sage.action_queue(id) ON DELETE SET NULL;


--
-- Name: findings findings_action_log_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.findings
    ADD CONSTRAINT findings_action_log_id_fkey FOREIGN KEY (action_log_id) REFERENCES sage.action_log(id) ON DELETE SET NULL;


--
-- Name: guard_cluster_roles guard_cluster_roles_principal_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_cluster_roles
    ADD CONSTRAINT guard_cluster_roles_principal_id_fkey FOREIGN KEY (principal_id) REFERENCES sage.guard_principals(id);


--
-- Name: guard_freezes guard_freezes_kill_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_freezes
    ADD CONSTRAINT guard_freezes_kill_id_fkey FOREIGN KEY (kill_id) REFERENCES sage.guard_kills(id);


--
-- Name: guard_identity_bindings guard_identity_bindings_principal_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_identity_bindings
    ADD CONSTRAINT guard_identity_bindings_principal_id_fkey FOREIGN KEY (principal_id) REFERENCES sage.guard_principals(id) ON DELETE CASCADE;


--
-- Name: guard_principal_files guard_principal_files_principal_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_principal_files
    ADD CONSTRAINT guard_principal_files_principal_id_fkey FOREIGN KEY (principal_id) REFERENCES sage.guard_principals(id) ON DELETE CASCADE;


--
-- Name: guard_principals guard_principals_retired_by_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_principals
    ADD CONSTRAINT guard_principals_retired_by_fkey FOREIGN KEY (retired_by) REFERENCES sage.users(id) ON DELETE SET NULL;


--
-- Name: guard_principals guard_principals_sponsor_user_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_principals
    ADD CONSTRAINT guard_principals_sponsor_user_id_fkey FOREIGN KEY (sponsor_user_id) REFERENCES sage.users(id) ON DELETE SET NULL;


--
-- Name: guard_taint guard_taint_principal_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_taint
    ADD CONSTRAINT guard_taint_principal_id_fkey FOREIGN KEY (principal_id) REFERENCES sage.guard_principals(id);


--
-- Name: guard_unfreeze_requests guard_unfreeze_requests_freeze_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.guard_unfreeze_requests
    ADD CONSTRAINT guard_unfreeze_requests_freeze_id_fkey FOREIGN KEY (freeze_id) REFERENCES sage.guard_freezes(id);


--
-- Name: incident_avoided incident_avoided_action_log_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.incident_avoided
    ADD CONSTRAINT incident_avoided_action_log_id_fkey FOREIGN KEY (action_log_id) REFERENCES sage.action_log(id);


--
-- Name: incident_avoided incident_avoided_decision_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.incident_avoided
    ADD CONSTRAINT incident_avoided_decision_id_fkey FOREIGN KEY (decision_id) REFERENCES sage.decision(id);


--
-- Name: incident_avoided incident_avoided_verification_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.incident_avoided
    ADD CONSTRAINT incident_avoided_verification_id_fkey FOREIGN KEY (verification_id) REFERENCES sage.verification(id);


--
-- Name: incidents incidents_previous_incident_fk; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.incidents
    ADD CONSTRAINT incidents_previous_incident_fk FOREIGN KEY (previous_incident_id) REFERENCES sage.incidents(id) ON DELETE SET NULL;


--
-- Name: mcp_tokens mcp_tokens_owner_user_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.mcp_tokens
    ADD CONSTRAINT mcp_tokens_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES sage.users(id) ON DELETE CASCADE;


--
-- Name: mcp_tokens mcp_tokens_principal_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.mcp_tokens
    ADD CONSTRAINT mcp_tokens_principal_id_fkey FOREIGN KEY (principal_id) REFERENCES sage.guard_principals(id);


--
-- Name: notification_log notification_log_channel_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_log
    ADD CONSTRAINT notification_log_channel_id_fkey FOREIGN KEY (channel_id) REFERENCES sage.notification_channels(id) ON DELETE SET NULL;


--
-- Name: notification_rules notification_rules_channel_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.notification_rules
    ADD CONSTRAINT notification_rules_channel_id_fkey FOREIGN KEY (channel_id) REFERENCES sage.notification_channels(id) ON DELETE CASCADE;


--
-- Name: policy_approvals policy_approvals_policy_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.policy_approvals
    ADD CONSTRAINT policy_approvals_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES sage.policy(id) ON DELETE CASCADE;


--
-- Name: policy policy_supersedes_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.policy
    ADD CONSTRAINT policy_supersedes_id_fkey FOREIGN KEY (supersedes_id) REFERENCES sage.policy(id);


--
-- Name: recommendation recommendation_action_log_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation
    ADD CONSTRAINT recommendation_action_log_id_fkey FOREIGN KEY (action_log_id) REFERENCES sage.action_log(id) ON DELETE SET NULL;


--
-- Name: recommendation recommendation_finding_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation
    ADD CONSTRAINT recommendation_finding_id_fkey FOREIGN KEY (finding_id) REFERENCES sage.findings(id) ON DELETE SET NULL;


--
-- Name: recommendation_revision recommendation_revision_recommendation_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation_revision
    ADD CONSTRAINT recommendation_revision_recommendation_id_fkey FOREIGN KEY (recommendation_id) REFERENCES sage.recommendation(id) ON DELETE CASCADE;


--
-- Name: recommendation_transition recommendation_transition_recommendation_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.recommendation_transition
    ADD CONSTRAINT recommendation_transition_recommendation_id_fkey FOREIGN KEY (recommendation_id) REFERENCES sage.recommendation(id) ON DELETE CASCADE;


--
-- Name: rollout_instance rollout_instance_run_evidence_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.rollout_instance
    ADD CONSTRAINT rollout_instance_run_evidence_id_fkey FOREIGN KEY (run_evidence_id) REFERENCES sage.rollout_run(evidence_id);


--
-- Name: schema_baseline schema_baseline_last_authorized_action_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.schema_baseline
    ADD CONSTRAINT schema_baseline_last_authorized_action_id_fkey FOREIGN KEY (last_authorized_action_id) REFERENCES sage.action_log(id) ON DELETE SET NULL;


--
-- Name: schema_baseline schema_baseline_last_authorized_decision_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.schema_baseline
    ADD CONSTRAINT schema_baseline_last_authorized_decision_id_fkey FOREIGN KEY (last_authorized_decision_id) REFERENCES sage.decision(id) ON DELETE SET NULL;


--
-- Name: sessions sessions_user_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES sage.users(id) ON DELETE CASCADE;


--
-- Name: sre_action_proposals sre_action_proposals_deployment_id_database_id_investigati_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_action_proposals
    ADD CONSTRAINT sre_action_proposals_deployment_id_database_id_investigati_fkey FOREIGN KEY (deployment_id, database_id, investigation_id) REFERENCES sage.sre_investigations(deployment_id, database_id, id) ON DELETE CASCADE;


--
-- Name: sre_budget_reservations sre_budget_reservations_deployment_id_database_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_budget_reservations
    ADD CONSTRAINT sre_budget_reservations_deployment_id_database_id_fkey FOREIGN KEY (deployment_id, database_id) REFERENCES sage.sre_database_bindings(deployment_id, database_id);


--
-- Name: sre_budget_reservations sre_budget_reservations_deployment_id_database_id_investig_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_budget_reservations
    ADD CONSTRAINT sre_budget_reservations_deployment_id_database_id_investig_fkey FOREIGN KEY (deployment_id, database_id, investigation_id) REFERENCES sage.sre_investigations(deployment_id, database_id, id);


--
-- Name: sre_events sre_events_deployment_id_database_id_investigation_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_events
    ADD CONSTRAINT sre_events_deployment_id_database_id_investigation_id_fkey FOREIGN KEY (deployment_id, database_id, investigation_id) REFERENCES sage.sre_investigations(deployment_id, database_id, id);


--
-- Name: sre_evidence sre_evidence_deployment_id_database_id_investigation_id_st_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_evidence
    ADD CONSTRAINT sre_evidence_deployment_id_database_id_investigation_id_st_fkey FOREIGN KEY (deployment_id, database_id, investigation_id, step_id) REFERENCES sage.sre_steps(deployment_id, database_id, investigation_id, id);


--
-- Name: sre_hypotheses sre_hypotheses_deployment_id_database_id_investigation_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_hypotheses
    ADD CONSTRAINT sre_hypotheses_deployment_id_database_id_investigation_id_fkey FOREIGN KEY (deployment_id, database_id, investigation_id) REFERENCES sage.sre_investigations(deployment_id, database_id, id);


--
-- Name: sre_investigation_outcomes sre_investigation_outcomes_deployment_id_database_id_inves_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_investigation_outcomes
    ADD CONSTRAINT sre_investigation_outcomes_deployment_id_database_id_inves_fkey FOREIGN KEY (deployment_id, database_id, investigation_id) REFERENCES sage.sre_investigations(deployment_id, database_id, id) ON DELETE CASCADE;


--
-- Name: sre_investigations sre_investigations_deployment_id_database_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_investigations
    ADD CONSTRAINT sre_investigations_deployment_id_database_id_fkey FOREIGN KEY (deployment_id, database_id) REFERENCES sage.sre_database_bindings(deployment_id, database_id);


--
-- Name: sre_maintenance_proposals sre_maintenance_proposals_deployment_id_database_id_invest_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_maintenance_proposals
    ADD CONSTRAINT sre_maintenance_proposals_deployment_id_database_id_invest_fkey FOREIGN KEY (deployment_id, database_id, investigation_id) REFERENCES sage.sre_investigations(deployment_id, database_id, id) ON DELETE CASCADE;


--
-- Name: sre_runbook_runs sre_runbook_runs_deployment_id_database_id_investigation_i_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_runbook_runs
    ADD CONSTRAINT sre_runbook_runs_deployment_id_database_id_investigation_i_fkey FOREIGN KEY (deployment_id, database_id, investigation_id) REFERENCES sage.sre_investigations(deployment_id, database_id, id) ON DELETE CASCADE;


--
-- Name: sre_runbook_runs sre_runbook_runs_deployment_id_database_id_runbook_id_vers_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_runbook_runs
    ADD CONSTRAINT sre_runbook_runs_deployment_id_database_id_runbook_id_vers_fkey FOREIGN KEY (deployment_id, database_id, runbook_id, version) REFERENCES sage.sre_runbook_versions(deployment_id, database_id, runbook_id, version);


--
-- Name: sre_runbook_versions sre_runbook_versions_deployment_id_database_id_runbook_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_runbook_versions
    ADD CONSTRAINT sre_runbook_versions_deployment_id_database_id_runbook_id_fkey FOREIGN KEY (deployment_id, database_id, runbook_id) REFERENCES sage.sre_runbooks(deployment_id, database_id, id);


--
-- Name: sre_runbooks sre_runbooks_deployment_id_database_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_runbooks
    ADD CONSTRAINT sre_runbooks_deployment_id_database_id_fkey FOREIGN KEY (deployment_id, database_id) REFERENCES sage.sre_database_bindings(deployment_id, database_id);


--
-- Name: sre_service_slos sre_service_slos_deployment_id_database_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_service_slos
    ADD CONSTRAINT sre_service_slos_deployment_id_database_id_fkey FOREIGN KEY (deployment_id, database_id) REFERENCES sage.sre_database_bindings(deployment_id, database_id);


--
-- Name: sre_slo_transitions sre_slo_transitions_deployment_id_database_id_name_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_slo_transitions
    ADD CONSTRAINT sre_slo_transitions_deployment_id_database_id_name_fkey FOREIGN KEY (deployment_id, database_id, name) REFERENCES sage.sre_service_slos(deployment_id, database_id, name) ON DELETE CASCADE;


--
-- Name: sre_steps sre_steps_deployment_id_database_id_investigation_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.sre_steps
    ADD CONSTRAINT sre_steps_deployment_id_database_id_investigation_id_fkey FOREIGN KEY (deployment_id, database_id, investigation_id) REFERENCES sage.sre_investigations(deployment_id, database_id, id);


--
-- Name: user_oidc_link_grants user_oidc_link_grants_user_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.user_oidc_link_grants
    ADD CONSTRAINT user_oidc_link_grants_user_id_fkey FOREIGN KEY (user_id) REFERENCES sage.users(id) ON DELETE CASCADE;


--
-- Name: verification verification_action_log_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.verification
    ADD CONSTRAINT verification_action_log_id_fkey FOREIGN KEY (action_log_id) REFERENCES sage.action_log(id) ON DELETE SET NULL;


--
-- Name: verification verification_decision_id_fkey; Type: FK CONSTRAINT; Schema: sage; Owner: -
--

ALTER TABLE ONLY sage.verification
    ADD CONSTRAINT verification_decision_id_fkey FOREIGN KEY (decision_id) REFERENCES sage.decision(id);


--
-- PostgreSQL database dump complete
--


