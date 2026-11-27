package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// warnIsolationDefault is the startup self-check behind the audit chain's
// read committed requirement (audit-02): one WARN when the role or
// database default of database name is another level. pg_sage's sessions
// are pinned to read committed, so the default only bites where a
// transaction pooler drops the pin. A failed read is reported too, never
// taken for a safe default.
func warnIsolationDefault(
	ctx context.Context, q auditchain.Querier, name string,
	warn func(component, msg string, args ...any),
) {
	msg, err := auditchain.IsolationWarning(ctx, q)
	switch {
	case err != nil:
		warn("audit-chain", "db %q: could not check the isolation default: %v", name, err)
	case msg != "":
		warn("audit-chain", "db %q: %s", name, msg)
	}
}
