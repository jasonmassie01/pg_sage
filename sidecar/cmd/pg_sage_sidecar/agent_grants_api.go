package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/agentguard/grants"
)

// lateGrantAPI is api.AgentGrantService over the lazily built grant
// service: without a control database every call is unavailable (503).
type lateGrantAPI struct{ late lateGrants }

func (l lateGrantAPI) Grants(ctx context.Context, db string, f grants.Filter) (grants.Page,
	error) {
	svc, err := l.late.svc()
	if err != nil {
		return grants.Page{}, err
	}
	return svc.Grants(ctx, db, f)
}

func (l lateGrantAPI) GrantNow(ctx context.Context, pid string, in grants.CapabilityRequest,
	user int) (grants.GrantResult, error) {
	svc, err := l.late.svc()
	if err != nil {
		return grants.GrantResult{}, err
	}
	return svc.GrantNow(ctx, pid, in, user)
}

func (l lateGrantAPI) RevokeNow(ctx context.Context, db, pid string, id int64,
	user int) (grants.RevokeResult, error) {
	svc, err := l.late.svc()
	if err != nil {
		return grants.RevokeResult{}, err
	}
	return svc.RevokeNow(ctx, db, pid, id, user)
}

func (l lateGrantAPI) Requests(ctx context.Context, db string,
	f grants.RequestFilter) (grants.RequestPage, error) {
	svc, err := l.late.svc()
	if err != nil {
		return grants.RequestPage{}, err
	}
	return svc.Requests(ctx, db, f)
}

func (l lateGrantAPI) Approve(ctx context.Context, db, pid string, id int64,
	user int) (grants.GrantResult, error) {
	svc, err := l.late.svc()
	if err != nil {
		return grants.GrantResult{}, err
	}
	return svc.Approve(ctx, db, pid, id, user)
}

func (l lateGrantAPI) Deny(ctx context.Context, db, pid string, id int64,
	user int) (grants.Request, error) {
	svc, err := l.late.svc()
	if err != nil {
		return grants.Request{}, err
	}
	return svc.Deny(ctx, db, pid, id, user)
}
