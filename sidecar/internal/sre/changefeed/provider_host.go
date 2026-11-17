package changefeed

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// hostState is the server_host source's snapshot: the server address
// ("" on a Unix socket), the cluster's system identifier ("" until it can
// be read) and how many host changes were seen, which keys each change so
// a flip back is a new event.
type hostState struct {
	Addr     string `json:"addr"`
	SystemID string `json:"system_id,omitempty"`
	Changes  int    `json:"changes,omitempty"`
}

// readHost reads the server address and, where pg_control_system() may be
// executed, the system identifier; without that privilege (or on a
// provider without the function) the source runs on the address alone.
func readHost(ctx context.Context, q querier) (hostState, error) {
	var addr *string
	if err := q.QueryRow(ctx, `/* pg_sage sre:change_feed */
		SELECT host(inet_server_addr())`).Scan(&addr); err != nil {
		return hostState{}, err
	}
	var st hostState
	if addr != nil {
		st.Addr = *addr
	}
	err := q.QueryRow(ctx, `/* pg_sage sre:change_feed */
		SELECT system_identifier::text FROM pg_control_system()`).Scan(&st.SystemID)
	switch reason := classify(err); {
	case err == nil:
	case reason == "no_privilege" || reason == "unsupported":
		st.SystemID = ""
	default:
		return hostState{}, err
	}
	return st, nil
}

// pollServerHost snapshots the server's address and system identifier. A
// change is reported only when both sides are known, so switching between
// a socket and TCP, or losing the identifier privilege, is not a host
// change; the last known identifier is kept.
func pollServerHost(ctx context.Context, q querier, prev []byte, now time.Time) ([]Event,
	any, error) {
	cur, err := readHost(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	if prev == nil {
		return nil, cur, nil
	}
	var old hostState
	if err := json.Unmarshal(prev, &old); err != nil {
		return nil, nil, fmt.Errorf("server_host state: %w", err)
	}
	if cur.SystemID == "" {
		cur.SystemID = old.SystemID
	}
	cur.Changes = old.Changes
	addrChanged := old.Addr != "" && cur.Addr != "" && old.Addr != cur.Addr
	idChanged := old.SystemID != "" && cur.SystemID != "" && old.SystemID != cur.SystemID
	if !addrChanged && !idChanged {
		return nil, cur, nil
	}
	cur.Changes++
	return []Event{hostEvent(old, cur, addrChanged, idChanged, now)}, cur, nil
}

func hostEvent(old, cur hostState, addrChanged, idChanged bool, now time.Time) Event {
	text := "server host changed:"
	if addrChanged {
		text += fmt.Sprintf(" address %s -> %s", old.Addr, cur.Addr)
	}
	if idChanged {
		text += fmt.Sprintf(" system identifier %s -> %s", old.SystemID, cur.SystemID)
	}
	text += " (failover, live migration or restore)"
	return internalEvent(KindHostChange,
		fmt.Sprintf("host:%d:%s->%s:%s->%s", cur.Changes, old.Addr, cur.Addr, old.SystemID,
			cur.SystemID), text, now, now)
}
