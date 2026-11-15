package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/alerting"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/doctor"
)

// writeDoctorText prints one block per check and the counts last.
func writeDoctorText(w io.Writer, r doctor.Report) {
	for _, c := range r.Checks {
		_, _ = fmt.Fprintf(w, "%-7s %s: %s\n", strings.ToUpper(string(c.Status)), c.Title,
			c.Detail)
		if c.Fix != "" {
			fix := strings.ReplaceAll(c.Fix, "\n", "\n             ")
			_, _ = fmt.Fprintf(w, "        fix: %s\n", fix)
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d pass, %d warn, %d fail, %d unknown (provider %s)\n",
		r.Counts.Pass, r.Counts.Warn, r.Counts.Fail, r.Counts.Unknown, r.Provider)
}

// doctorConfigSecrets are the config values doctor masks wherever they
// would appear: database passwords, the model key and alert targets.
func doctorConfigSecrets(cfg *config.Config) []string {
	out := []string{cfg.Postgres.Password, cfg.LLM.APIKey, cfg.Alerting.SlackWebhookURL,
		cfg.Alerting.PagerDutyRoutingKey}
	for _, wh := range cfg.Alerting.Webhooks {
		out = append(out, wh.URL)
		for _, v := range wh.Headers {
			out = append(out, v)
		}
	}
	return out
}

// doctorLog writes the clients' log lines to stderr, masked, so --json
// keeps stdout to the report.
func doctorLog(stderr io.Writer, secrets []string) func(string, string, ...any) {
	return func(component, msg string, args ...any) {
		line := doctor.Mask(fmt.Sprintf(msg, args...), secrets)
		_, _ = fmt.Fprintf(stderr, "%s [%s] %s\n", time.Now().UTC().Format(time.RFC3339),
			component, line)
	}
}

// targetURL hides a delivery error's URLs: webhook URLs carry secrets.
var targetURL = regexp.MustCompile(`https?://[^\s"']+`)

// doctorAlertSender sends one info-severity test alert through each
// channel the alerting: block configures (the same builder the sidecar
// uses, under the notification target policy).
func doctorAlertSender(cfg *config.Config,
	logFn func(string, string, ...any)) doctor.NotifyTest {
	return func(ctx context.Context) (int, error) {
		channels := buildAlertChannels(cfg, logFn)
		names := make([]string, 0, len(channels))
		for name := range channels {
			names = append(names, name)
		}
		sort.Strings(names)
		alert := doctorTestAlert(time.Now().UTC())
		sent := 0
		var errs []error
		for _, name := range names {
			if err := channels[name].Send(ctx, alert); err != nil {
				errs = append(errs, fmt.Errorf("%s: %s", name,
					targetURL.ReplaceAllString(err.Error(), "<url>")))
				continue
			}
			sent++
		}
		return sent, errors.Join(errs...)
	}
}

func doctorTestAlert(now time.Time) alerting.Alert {
	return alerting.Alert{Severity: "info", Timestamp: now, Findings: []alerting.AlertFinding{{
		Category: "test", Severity: "info", Title: "pg_sage doctor test notification",
		ObjectType: "doctor", ObjectIdentifier: "doctor",
		Recommendation: "Nothing to do: pg_sage doctor --send-test sent this.",
		FirstSeen:      now, LastSeen: now, OccurrenceCount: 1,
	}}}
}
