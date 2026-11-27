package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/provideradvice"
)

// Provider advice (market-09). GET /api/v1/provider-advice?database=
// lists, per database, the managed provider's own advisor recommendations
// (Cloud SQL / AlloyDB index advisor, Azure index tuning, Supabase
// advisors) with pg_sage's verdict, and each source's last fetch
// (?include_resolved=true adds advice the provider no longer gives).

const providerAdviceMeaning = "The provider's own advisor recommendations beside pg_sage's: " +
	"agree (pg_sage proposes a change that serves it), pgsage_only, provider_only (with " +
	"whether pg_sage would act and why, from its own what-if), conflict (for example a " +
	"drop of an index that backs a constraint or is scanned), unparsed (kept as text, " +
	"never offered as SQL) and unknown. Provider advice is evidence only: it never raises " +
	"confidence, risk tier or autonomy, and any action is an ordinary pg_sage candidate " +
	"through the policy gate."

func registerProviderAdviceRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	routeAggregate(mux, "GET /api/v1/provider-advice", gateViewer, mgr,
		aggregateHandler(providerAdviceHandler))
}

// providerAdviceDatabase is one database's advice and its sources.
type providerAdviceDatabase struct {
	Database string                        `json:"database"`
	Advice   []provideradvice.Record       `json:"advice"`
	Sources  []provideradvice.SourceStatus `json:"sources"`
}

func providerAdviceHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		includeResolved := false
		if raw := r.URL.Query().Get("include_resolved"); raw != "" {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				jsonError(w, "include_resolved must be true or false", http.StatusBadRequest)
				return
			}
			includeResolved = v
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		out := []providerAdviceDatabase{}
		for _, np := range poolsForDatabaseSelection(mgr, database) {
			entry, err := readProviderAdvice(r.Context(), np, includeResolved)
			if err != nil {
				internalError(w, r, "read provider advice of "+np.name, err)
				return
			}
			out = append(out, entry)
		}
		jsonResponse(w, map[string]any{"databases": out, "meaning": providerAdviceMeaning})
	}
}

func readProviderAdvice(ctx context.Context, np namedPool, includeResolved bool,
) (providerAdviceDatabase, error) {
	store := provideradvice.NewStore(np.pool)
	advice, err := store.List(ctx, np.name, includeResolved, 500)
	if err != nil {
		return providerAdviceDatabase{}, err
	}
	sources, err := store.Sources(ctx, np.name)
	if err != nil {
		return providerAdviceDatabase{}, err
	}
	if sources == nil {
		sources = []provideradvice.SourceStatus{}
	}
	return providerAdviceDatabase{Database: np.name, Advice: advice, Sources: sources}, nil
}

// enrichCaseProviderAdvice attaches the open provider advice behind each
// finding case, read in one query for the database. A failed read is
// logged and marks every finding case provider_advice_unavailable.
func enrichCaseProviderAdvice(ctx context.Context, projected []cases.Case,
	selected namedPool, findingIDs []int) {
	if len(findingIDs) == 0 || selected.pool == nil {
		return
	}
	ids := make([]int64, len(findingIDs))
	for i, id := range findingIDs {
		ids[i] = int64(id)
	}
	byFinding, err := provideradvice.NewStore(selected.pool).ForFindings(ctx, selected.name,
		ids)
	for i := range projected {
		findingID, ok := caseFindingID(projected[i])
		switch {
		case !ok:
		case err != nil:
			cases.MarkProviderAdviceUnavailable(&projected[i])
		case len(byFinding[int64(findingID)]) > 0:
			cases.AttachProviderAdvice(&projected[i], caseAdvice(byFinding[int64(findingID)]))
		}
	}
	if err != nil {
		slog.Warn("case provider advice read failed; marked provider_advice_unavailable",
			"database", selected.name, "findings", len(ids), "error", err)
	}
}

func caseAdvice(recs []provideradvice.Record) []cases.ProviderAdvice {
	out := make([]cases.ProviderAdvice, 0, len(recs))
	for _, r := range recs {
		out = append(out, cases.ProviderAdvice{Provider: r.Provider, Kind: string(r.Kind),
			Verdict: r.Verdict, Reason: r.Reason, Object: r.Payload.Object,
			LastSeen: r.LastSeen})
	}
	return out
}
