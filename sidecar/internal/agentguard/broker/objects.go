package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// CatalogObjects is a minimal decide.ObjectChecker (D5) for the brokered
// read path, used until the grants workstream's checker is wired: every
// object must resolve to a relation (agent_query resolves names on its
// search path first), and a named column must not be secret, nor pii in
// stage or prod. Column grants stay the control: an ungranted column
// fails with 42501, and output classes are checked again per column.
type CatalogObjects struct {
	Targets Targets
	Classes Classes
}

var errUnresolved = errors.New("object does not resolve")

// CheckObjects implements decide.ObjectChecker.
func (c CatalogObjects) CheckObjects(ctx context.Context, _ agentguard.Principal,
	database string, env envbind.Env, objects []decide.Object) error {
	t, err := c.Targets.Target(ctx, database)
	if err != nil {
		return err
	}
	for _, o := range objects {
		relid, err := resolveObject(ctx, t, o)
		if errors.Is(err, errUnresolved) {
			return &agentguard.DeniedError{Reason: decide.ReasonClassification,
				Detail: fmt.Sprintf("%s does not resolve to a relation", objectName(o)),
				Fix:    "name an existing table or view, schema-qualified"}
		}
		if err != nil {
			return err
		}
		if o.Column == "" {
			continue
		}
		if err := c.checkColumn(ctx, t, env, relid, o); err != nil {
			return err
		}
	}
	return nil
}

func (c CatalogObjects) checkColumn(ctx context.Context, t Target, env envbind.Env,
	relid uint32, o decide.Object) error {
	var attnum int16
	err := t.Pool.QueryRow(ctx, `/* pg_sage agent_query v1 */
		SELECT attnum FROM pg_catalog.pg_attribute
		WHERE attrelid = $1::oid AND attname = $2 AND attnum > 0 AND NOT attisdropped`,
		relid, o.Column).Scan(&attnum)
	if errors.Is(err, pgx.ErrNoRows) {
		return &agentguard.DeniedError{Reason: decide.ReasonClassification,
			Detail: objectName(o) + " does not resolve to a column"}
	}
	if err != nil {
		return fmt.Errorf("resolve %s: %w", objectName(o), err)
	}
	rc, err := c.Classes.Lookup(ctx, t, relid)
	if err != nil {
		return err
	}
	if act := decideColumn(env, rc.Of(attnum).Class, false); act == actDeny {
		return &agentguard.DeniedError{Reason: decide.ReasonClassification,
			Detail: fmt.Sprintf("%s is classified %s", objectName(o), rc.Of(attnum).Class),
			Fix:    "an operator adds an agents.unmask entry, or select other columns"}
	}
	return nil
}

func resolveObject(ctx context.Context, t Target, o decide.Object) (uint32, error) {
	if o.Schema == "" {
		return 0, errUnresolved
	}
	var relid *uint32
	err := t.Pool.QueryRow(ctx, `/* pg_sage agent_query v1 */
		SELECT pg_catalog.to_regclass(pg_catalog.format('%I.%I', $1::text, $2::text))::oid`,
		o.Schema, o.Relation).Scan(&relid)
	if err != nil {
		return 0, fmt.Errorf("resolve %s: %w", objectName(o), err)
	}
	if relid == nil {
		return 0, errUnresolved
	}
	return *relid, nil
}

func objectName(o decide.Object) string {
	name := o.Relation
	if o.Schema != "" {
		name = o.Schema + "." + name
	}
	if o.Column != "" {
		name += "." + o.Column
	}
	return name
}

// Compile-time check: CatalogObjects is a decide.ObjectChecker.
var _ decide.ObjectChecker = CatalogObjects{}
