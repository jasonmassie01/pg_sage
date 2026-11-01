package sre

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func (s *investigatorSession) historyTool() agentloop.Tool {
	var ids []string
	for _, id := range probes.HistoricalIDs() {
		if s.c.SourceAllowed(id) {
			ids = append(ids, `"`+string(id)+`"`)
		}
	}
	return agentloop.Tool{Name: ToolHistory, Cost: 1,
		Description: "Read bounded historical Lakebase observations by original event time, including log RCA signals and DDL changes. Historical samples are incomplete; empty history never proves health, current state, SLO compliance, or exact error counts. Results are citable evidence with source/event identities, not live counter deltas.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"source":{"type":"string","enum":[` + strings.Join(ids, ",") + `]},"window_seconds":{"type":"integer","minimum":60,"maximum":604800},"queryid":{"type":"integer"}},"required":["source"]}`),
		Run: func(ctx context.Context, raw json.RawMessage) (agentloop.Output, error) {
			var a struct {
				Source  probes.ID `json:"source"`
				Window  int64     `json:"window_seconds"`
				QueryID int64     `json:"queryid"`
			}
			if json.Unmarshal(raw, &a) != nil || a.Window < 0 || a.Window > 604800 {
				return agentloop.Output{}, fmt.Errorf("%w: invalid historical arguments", agentloop.ErrInvalidArgs)
			}
			args := probes.Args{Window: time.Duration(a.Window) * time.Second, QueryID: a.QueryID}
			if err := probes.CheckHistoricalArgs(a.Source, args); err != nil {
				return agentloop.Output{}, fmt.Errorf("%w: %v", agentloop.ErrInvalidArgs, err)
			}
			return s.collect(ctx, func(ctx context.Context) probes.Result {
				if denied := probes.CheckReadPolicy(ctx, s.c.readPolicy, a.Source, args); denied != nil {
					return *denied
				}
				return s.c.investigator.History.ReadHistory(probes.WithReadPolicy(ctx, s.c.readPolicy), a.Source, args)
			})
		}}
}
