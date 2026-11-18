package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/pg-sage/sidecar/internal/upgradecheck"
)

// upgrade_readiness (market-05) checks the resolved database for a
// major-version upgrade: each check's pass/warn/block/unknown verdict with
// its evidence. It only reads (one bounded read-only transaction) and
// writes nothing. With brief: true (drill-02a) it returns the upgrade
// brief instead, which pg_sage keeps in the database's sage schema when it
// changed; target_major is then optional (the newest major).

// UpgradeReadinessRequest are the typed arguments of upgrade_readiness.
type UpgradeReadinessRequest struct {
	TargetMajor    int    `json:"target_major"`
	Method         string `json:"method,omitempty"`
	TargetDatabase string `json:"target_database,omitempty"`
	// Brief asks for the upgrade brief (drill-02a) instead of the checks.
	Brief bool `json:"brief,omitempty"`
	// Database is the resolved database the server injects; backends read
	// DatabaseFromContext.
	Database string `json:"database,omitempty"`
}

// UpgradeReadinessBackend serves upgrade_readiness for the database the
// server resolved (DatabaseFromContext).
type UpgradeReadinessBackend interface {
	UpgradeReadiness(context.Context, UpgradeReadinessRequest) (any, error)
}

var upgradeToolNames = map[string]bool{"upgrade_readiness": true}

func upgradeTools() []Tool {
	return []Tool{{Name: "upgrade_readiness", Description: "Check the database for a " +
		"major-version upgrade: tables logical replication cannot identify rows of, " +
		"extensions against a target member, logical slots and subscriptions, data " +
		"checksums against PG18's initdb default, collation versions and indexes to " +
		"reindex, removed settings and dated support windows. Each check is pass, warn, " +
		"block or unknown with up to 50 evidence rows. Read-only on the database. With " +
		"brief: true it returns the upgrade brief instead: blockers, extension path, " +
		"slot and CDC plan, collation reindex list, cost of waiting and an ordered " +
		"checklist (the upgrade itself stays with the operator); pg_sage keeps a " +
		"changed brief in its own sage schema",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"target_major":{"type":"integer","minimum":1,"description":"the major to ` +
			`upgrade to (above the server's, at most the newest pg_sage knows); ` +
			`optional with brief"},` +
			`"brief":{"type":"boolean","description":"return the upgrade brief"},` +
			`"method":{"type":"string","enum":["pg_upgrade","blue_green","logical"],` +
			`"description":"default pg_upgrade"},` +
			`"target_database":{"type":"string","maxLength":63,"description":"a ` +
			`monitored database already at target_major to compare extensions against"}},` +
			`"additionalProperties":false}`)}}
}

func (s *Server) callUpgradeTool(ctx context.Context, _ string,
	raw json.RawMessage) (any, *rpcError) {
	var req UpgradeReadinessRequest
	if !decodeStrict(raw, &req) {
		return nil, failure(codeInvalidParams, "invalid arguments: upgrade_readiness takes "+
			"target_major, method, target_database and brief")
	}
	target := upgradecheck.Target{Major: req.TargetMajor, Method: req.Method}
	if req.Brief && req.TargetMajor == 0 {
		target.Major = upgradecheck.DefaultTargetMajor(0) // checks the method alone
	}
	if err := target.ValidateStatic(); err != nil {
		return nil, failure(codeInvalidParams, "invalid arguments: "+err.Error())
	}
	if validateTargetDatabase(req.TargetDatabase) != nil {
		return nil, failure(codeInvalidParams, "invalid arguments: target_database must "+
			"be a monitored database name")
	}
	backend, ok := s.backend.(UpgradeReadinessBackend)
	if !ok {
		return nil, failure(codeInternal, "upgrade readiness unavailable")
	}
	result, err := backend.UpgradeReadiness(ctx, req)
	switch {
	case errors.Is(err, upgradecheck.ErrInvalidTarget):
		return nil, failure(codeInvalidParams, "invalid arguments: "+err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return nil, failure(codeCancelled, "request cancelled")
	case err != nil:
		return nil, failure(codeInternal, "upgrade readiness not checked: "+err.Error())
	}
	return result, nil
}

// validateTargetDatabase accepts "" or a plain database name.
func validateTargetDatabase(name string) error {
	if len(name) > 63 {
		return errors.New("too long")
	}
	for _, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-' || c == '.'
		if !ok {
			return errors.New("invalid character")
		}
	}
	return nil
}

// UpgradeReadiness serves upgrade_readiness.
func (backend *ProductionBackend) UpgradeReadiness(ctx context.Context,
	request UpgradeReadinessRequest) (any, error) {
	if backend.dependencies.UpgradeReadiness == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.UpgradeReadiness.UpgradeReadiness(ctx, request)
}
