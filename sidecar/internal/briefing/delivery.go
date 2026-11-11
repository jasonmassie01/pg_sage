package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
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

// Dispatch sends the briefing to the configured channels and reports each
// channel's outcome. The ctx bounds the outbound calls (Slack) so that
// graceful shutdown cancels in-flight dispatches.
func (w *Worker) Dispatch(ctx context.Context, briefing string) map[string]Delivery {
	cfg := w.settings().cfg
	out := make(map[string]Delivery, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		switch ch {
		case "stdout":
			out[ch] = w.writeStdoutChannel(briefing)
		case "slack":
			if cfg.SlackWebhookURL == "" {
				out[ch] = delivered(DeliverySkipped, "no briefing.slack_webhook_url configured")
				continue
			}
			out[ch] = w.postSlack(ctx, cfg.SlackWebhookURL, briefing)
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

// sendSlack posts text to the configured webhook.
func (w *Worker) sendSlack(ctx context.Context, text string) Delivery {
	return w.postSlack(ctx, w.settings().cfg.SlackWebhookURL, text)
}

// postSlack posts text as Slack mrkdwn and reports the outcome.
func (w *Worker) postSlack(ctx context.Context, hook, text string) Delivery {
	payload, err := json.Marshal(map[string]string{"text": toSlackMrkdwn(text)})
	if err != nil {
		return delivered(DeliveryError, "encode slack payload: "+err.Error())
	}
	// Cap the Slack POST at 10s, but inherit the caller's context so
	// process shutdown cancels the request.
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, "POST", hook,
		strings.NewReader(string(payload)))
	if err != nil {
		cause := webhookFailureCause(err)
		w.logFn("WARN", "briefing: slack request error: %v", cause)
		return delivered(DeliveryError, "slack request: "+cause.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := briefingHTTPClient.Do(req)
	if err != nil {
		cause := webhookFailureCause(err)
		w.logFn("WARN", "briefing: slack send error: %v", cause)
		return delivered(DeliveryError, "slack send: "+cause.Error())
	}
	_ = resp.Body.Close()
	d := delivered(DeliverySent, "")
	d.HTTPStatus = resp.StatusCode
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		w.logFn("WARN", "briefing: slack delivery rejected: HTTP %d", resp.StatusCode)
		d.Status = DeliveryError
		d.Error = fmt.Sprintf("slack rejected the briefing: HTTP %d", resp.StatusCode)
	}
	return d
}

// A webhook URL contains its credential; url.Error includes that URL in Error().
// Preserve the underlying failure without copying the credential into logs.
func webhookFailureCause(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
