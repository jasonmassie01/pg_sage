package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
)

const titleModel = "Model key"

// modelProbeTimeout bounds the whole probe, retries included.
const modelProbeTimeout = 90 * time.Second

// detailNoModel is the model check's detail when none is configured.
const detailNoModel = "no model configured: pg_sage runs on deterministic rules alone " +
	"until llm.endpoint, llm.api_key and llm.model are set"

// modelCheck sends one tiny request through the configured client, the
// request POST /api/v1/llm/test sends, and maps its class to a status.
func modelCheck(ctx context.Context, probe ModelProbe, skipped string) Check {
	if skipped != "" {
		return unknownCheck(IDModel, titleModel, skipped)
	}
	if probe == nil {
		return unknownCheck(IDModel, titleModel, detailNoModel)
	}
	pctx, cancel := context.WithTimeout(ctx, modelProbeTimeout)
	defer cancel()
	return modelResultCheck(probe(pctx))
}

// modelResultCheck maps a probe result: each failure class gets its own
// detail and the setting to change.
func modelResultCheck(res llm.ProbeResult) Check {
	c := Check{ID: IDModel, Title: titleModel, Status: StatusFail}
	cause := ""
	if res.Error != "" {
		cause = " (" + res.Error + ")"
	}
	switch res.Class {
	case "ok":
		c.Status = StatusPass
		c.Detail = fmt.Sprintf("model %s answered in %d ms", res.Model, res.LatencyMS)
	case "disabled":
		return unknownCheck(IDModel, titleModel, detailNoModel)
	case "canceled":
		return unknownCheck(IDModel, titleModel, "the probe was canceled before an answer")
	case "budget":
		c.Status = StatusWarn
		c.Detail = "today's token budget is used up, so the probe was not sent"
		c.Fix = "raise llm.token_budget_daily or wait for the daily reset"
	case "malformed":
		c.Detail, c.Fix = malformedDetail(res.Error), "point llm.endpoint at an "+
			"OpenAI-compatible chat completions API (the base URL ending in /v1)"
	default:
		c.Detail, c.Fix = failureDetail(res.Class, res.Model)
		c.Detail += cause
	}
	return c
}

// failureDetail is what each non-200 class means and what to change.
func failureDetail(class, model string) (string, string) {
	switch class {
	case "auth":
		return "the provider refused the key (401/403)",
			"check llm.api_key (or ${LLM_API_KEY}) and that the key may use the model"
	case "rate":
		return "the provider's rate limit answered 429 on every attempt",
			"wait and run again, or raise the key's quota or tier with the provider"
	case "timeout":
		return "the provider did not answer within llm.timeout_seconds",
			"check the endpoint is reachable from this host, or raise llm.timeout_seconds"
	case "transport":
		return "could not reach llm.endpoint",
			"check llm.endpoint's host and port, DNS, proxy and firewall from this host"
	case "caller":
		return fmt.Sprintf("the provider rejected the request for model %q", model),
			"check llm.model names a chat model this endpoint serves"
	case "server":
		return "the provider answered with a server error (5xx) on every attempt",
			"check the provider's status page, then run again"
	case "circuit_open":
		return "recent failures opened the circuit breaker",
			"fix the cause in the sidecar log, then POST /api/v1/llm/test"
	case "empty":
		return "the model answered with no text",
			"check llm.model is a chat model, not an embedding or completion model"
	}
	return "the probe failed", "check llm.endpoint, llm.api_key and llm.model"
}

// malformedDetail tells apart the 200 replies that are not a completion.
func malformedDetail(errText string) string {
	switch {
	case strings.Contains(errText, "unexpected end of JSON input"):
		return "the endpoint answered 200 with an empty reply (or one cut short)"
	case strings.Contains(errText, "invalid character '`'"):
		return "the endpoint answered with JSON inside a markdown fence, not an API " +
			"response: it looks like a chat front end, not the API"
	case strings.Contains(errText, "no choices"):
		return "the endpoint answered JSON with no choices"
	}
	return "the endpoint answered 200 with a body that is not valid JSON (" + errText + ")"
}
