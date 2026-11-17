package workpack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/cost"
)

// provider.json, stats.json and context.json.

// hostRow is the provider resource's latest hour of cloud telemetry.
type hostRow struct {
	class      string
	vcpus      int
	serverless bool
	mem        float64
	storage    float64
}

// readProvider reads the resource's latest host usage hour of the window
// (cost.LoadHours); no resource or no row is unknown, never inferred.
func readProvider(ctx context.Context, tx pgx.Tx, in catalogInput, cat *catalogData) error {
	if in.resource == "" {
		return nil
	}
	rows, err := cost.LoadHours(ctx, tx, in.resource, in.from)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !r.Hour.Before(in.to) {
			break
		}
		cat.host = &hostRow{class: r.InstanceClass, vcpus: r.VCPUs, serverless: r.Serverless,
			mem: r.MemoryTotalBytes, storage: r.AllocatedStorageBytes}
	}
	return nil
}

// buildProvider is the status fact and, when a row is known, one fact per
// field. A failed read is "unavailable" with its error class.
func buildProvider(resource string, row *hostRow, err error) Provider {
	status := func(v string) []ProviderFact {
		return []ProviderFact{{ID: "provider/status", Name: "status", Value: v}}
	}
	switch {
	case err != nil:
		return Provider{Status: valueUnavailable, ErrorClass: errorClass(err),
			Facts: status(valueUnavailable)}
	case resource == "" || row == nil:
		return Provider{Status: valueUnknown, Facts: status(valueUnknown)}
	}
	facts := status(relStatusOK)
	add := func(name string, v any) {
		facts = append(facts, ProviderFact{ID: "provider/" + name, Name: name, Value: v})
	}
	add("instance_class", knownString(row.class))
	add("vcpus", knownPositive(float64(row.vcpus), row.vcpus))
	add("memory_bytes", knownPositive(row.mem, row.mem))
	add("storage_bytes", knownPositive(row.storage, row.storage))
	add("serverless", row.serverless)
	return Provider{Status: relStatusOK, Facts: facts}
}

func knownString(s string) any {
	if s == "" {
		return valueUnknown
	}
	return s
}

func knownPositive(n float64, v any) any {
	if n <= 0 {
		return valueUnknown
	}
	return v
}

// errorClass names what kind of failure err is, without its message.
func errorClass(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr):
		return "sqlstate " + pgErr.Code
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	}
	return "error"
}

// factRow is one confirmed fact.
type factRow struct {
	typ, kind, subject string
	value              map[string]string
	by                 string
	at                 time.Time
	expires            *time.Time
}

var contextSQL = `/* pg_sage */ SELECT fact_type, subject_kind, subject, value::text,
       COALESCE(decided_by, ''), decided_at, expires_at
  FROM sage.facts
 WHERE fact_type <> 'column_class' AND status = 'confirmed' AND decided_at IS NOT NULL
   AND (expires_at IS NULL OR expires_at > $1)
 ORDER BY fact_type, subject_kind, subject
 LIMIT ` + fmt.Sprint(MaxContextFacts)

func readContext(ctx context.Context, tx pgx.Tx, in catalogInput, cat *catalogData) error {
	rows, err := tx.Query(ctx, contextSQL, in.now)
	if err != nil {
		return err
	}
	cat.facts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (factRow, error) {
		var f factRow
		var value string
		if err := row.Scan(&f.typ, &f.kind, &f.subject, &value, &f.by, &f.at,
			&f.expires); err != nil {
			return f, err
		}
		if err := json.Unmarshal([]byte(value), &f.value); err != nil {
			return f, fmt.Errorf("decode the value of fact %s %s: %w", f.typ, f.subject, err)
		}
		return f, nil
	})
	return err
}

// buildContext is the brief (or absent) and the other confirmed facts.
func buildContext(rows []factRow) Context {
	out := Context{Brief: map[string]string{"id": "context/brief", "brief": valueAbsent},
		Facts: []ContextFact{}}
	for _, r := range rows {
		f := ContextFact{Type: r.typ, SubjectKind: r.kind, Subject: r.subject,
			Value: r.value, ConfirmedBy: r.by, ConfirmedAt: r.at.UTC().Format(time.RFC3339)}
		if f.Value == nil {
			f.Value = map[string]string{}
		}
		if r.expires != nil {
			f.ExpiresAt = r.expires.UTC().Format(time.RFC3339)
		}
		if r.typ == "owner_brief" {
			f.ID = "context/brief"
			out.Brief = f
			continue
		}
		f.ID = "context/fact/" + strings.Join([]string{r.typ, r.kind, r.subject}, ".")
		out.Facts = append(out.Facts, f)
	}
	sort.SliceStable(out.Facts, func(a, b int) bool { return out.Facts[a].ID < out.Facts[b].ID })
	return out
}
