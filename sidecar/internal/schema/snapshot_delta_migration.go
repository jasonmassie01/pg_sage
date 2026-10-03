package schema

// Snapshot dedupe (dogfood lifeos: sage.snapshots reached 9.3 GB because
// every cycle stored each catalog list in full). A delta row names its base
// row in base_id and holds only what changed; legacy rows and keyframes keep
// base_id NULL and are read as before. Nothing existing is rewritten:
// adding a nullable column is a catalog-only change, and old full rows age
// out through retention.
//
// sage.snapshot_apply(base, delta) rebuilds a delta row's document from its
// base document; sage.snapshot_data(data, base_id) is the accessor every
// reader uses (a base may itself be a delta on a keyframe: the writer keeps
// chains at most two deep). The partial index serves retention's "is this
// row still a base" check. Idempotent.
const ddlSnapshotDelta = `
DO $$
BEGIN
    -- Checked first: ALTER TABLE would take ACCESS EXCLUSIVE on every bootstrap.
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
                   WHERE attrelid = 'sage.snapshots'::regclass
                     AND attname = 'base_id' AND NOT attisdropped) THEN
        ALTER TABLE sage.snapshots ADD COLUMN base_id bigint;
    END IF;
END
$$;
CREATE INDEX IF NOT EXISTS idx_snapshots_base
    ON sage.snapshots (base_id) WHERE base_id IS NOT NULL;

CREATE OR REPLACE FUNCTION sage.snapshot_apply(p_base jsonb, p_delta jsonb)
RETURNS jsonb LANGUAGE plpgsql IMMUTABLE STRICT PARALLEL SAFE AS $fn$
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
$fn$;

CREATE OR REPLACE FUNCTION sage.snapshot_data(p_data jsonb, p_base_id bigint)
RETURNS jsonb LANGUAGE plpgsql STABLE PARALLEL SAFE AS $fn$
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
$fn$;
`
