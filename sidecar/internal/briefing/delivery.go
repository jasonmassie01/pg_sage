package briefing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// Delivery outcomes stored in sage.briefings.delivery_status (llm-06).
const (
	DeliverySent    = "sent"
	DeliveryError   = "error"
	DeliverySkipped = "skipped"
)

// Delivery is one channel's outcome. Error never holds a webhook URL.
type Delivery struct {
	Status     string    `json:"status"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
}

func delivered(status, cause string) Delivery {
	return Delivery{Status: status, Error: cause, At: time.Now().UTC()}
}

// Dispatch writes the digest to the local briefing channels and reports
// each one's outcome. "slack" is not posted here: briefing.slack_webhook_url
// is reconciled into the managed Notifications channel
// config:slack:briefing (ux-02b), which reports under its own name.
func (w *Worker) Dispatch(_ context.Context, digest string) map[string]Delivery {
	cfg := w.settings().cfg
	out := make(map[string]Delivery, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		switch ch {
		case "stdout":
			out[ch] = w.writeStdoutChannel(digest)
		case "slack":
			continue // delivered through Notifications
		default:
			out[ch] = delivered(DeliverySkipped, "unknown briefing channel")
		}
	}
	return out
}

// RecordDelivery stores the dispatch outcome on the briefing row id.
func (w *Worker) RecordDelivery(
	ctx context.Context, id int64, status map[string]Delivery,
) error {
	if id <= 0 || w.pool == nil {
		return fmt.Errorf("record briefing delivery: no stored briefing row (id %d)", id)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("record briefing delivery: encode status: %w", err)
	}
	_, err = w.pool.Exec(ctx, `/* pg_sage */
		UPDATE sage.briefings SET delivery_status = $1 WHERE id = $2`,
		json.RawMessage(raw), id)
	if err != nil {
		return fmt.Errorf("record briefing delivery for row %d: %w", id, err)
	}
	return nil
}

// writeStdoutChannel prints the briefing for the "stdout" channel. When
// the MCP server speaks JSON-RPC over stdio, stdout is that protocol
// stream and any other line corrupts it, so the briefing goes to stderr
// and the operator is told once.
func (w *Worker) writeStdoutChannel(briefing string) Delivery {
	out, errOut := w.streams()
	mcp := w.mcpConfig()
	if mcp.Enabled && mcp.Transport == "stdio" {
		w.stdioNotice.Do(func() {
			w.logFn("WARN", "briefing: MCP stdio transport owns stdout; "+
				"the stdout briefing channel is written to stderr instead")
		})
		out = errOut
	}
	if _, err := fmt.Fprintln(out, briefing); err != nil {
		w.logFn("WARN", "briefing: write stdout channel: %v", err)
		return delivered(DeliveryError, "write stdout channel: "+err.Error())
	}
	return delivered(DeliverySent, "")
}

func (w *Worker) streams() (io.Writer, io.Writer) {
	out, errOut := w.stdout, w.stderr
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	return out, errOut
}
