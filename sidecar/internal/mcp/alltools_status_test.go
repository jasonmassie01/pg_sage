package mcp

import "context"

// The request status tools of allToolsBackend (mcp-01b): a final item the
// census caller (an admin person over MCP) itself proposed.

func init() {
	allToolsArgs[toolGetRequestStatus] = `{"handle":"plan:3"}`
	allToolsArgs[toolWaitForDecision] = `{"handle":"plan:3","timeout_seconds":1}`
}

func (b *allToolsBackend) RequestStatus(ctx context.Context, _ RequestQuery) (RequestRecord,
	error) {
	b.hit(ctx, toolGetRequestStatus)
	return RequestRecord{State: "rejected", DecidedByKind: byPerson, Final: true,
		OwnerActor: ActorFromContext(ctx)}, nil
}
