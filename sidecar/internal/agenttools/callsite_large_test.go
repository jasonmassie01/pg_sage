package agenttools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// A file larger than the client reads (a Rails db/structure.sql names every
// table, so it is a likely search hit) is skipped like a vanished one, not
// a failure of the whole lookup.

// largeFileHost answers the code search with hits and the contents API
// with a too-large answer for large and the content of files.
func largeFileHost(t *testing.T, hits []string, large string,
	files map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/search/code":
			items := make([]string, 0, len(hits))
			for _, h := range hits {
				items = append(items, `{"path":"`+h+`"}`)
			}
			_, _ = w.Write([]byte(`{"items":[` + strings.Join(items, ",") + `]}`))
		case path == "/repos/acme/shop/contents/"+large:
			_, _ = w.Write([]byte(`{"type":"file","encoding":"none","size":5242880,` +
				`"content":""}`))
		default:
			content, ok := files[strings.TrimPrefix(path, "/repos/acme/shop/contents/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_, _ = w.Write(contentsAnswer(content))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFindCallSiteSkipsASearchHitTooLargeToRead(t *testing.T) {
	sql := "SELECT id FROM orders WHERE customer_id = $1"
	srv := largeFileHost(t, []string{"db/structure.sql", "app/report.go"},
		"db/structure.sql", map[string]string{
			"app/report.go": "package app\n\nconst q = \"SELECT id FROM orders " +
				"WHERE customer_id = ?\"\n"})
	got, err := FindCallSite(context.Background(), githubClient(t, srv, 0), csRepo, 9,
		nil, sql)
	require.NoError(t, err, "a too-large hit fails the lookup")
	require.Equal(t, CallSiteExact, got.Confidence)
	require.Equal(t, CallSiteMethodLiteral, got.Method)
	require.Equal(t, "app/report.go", got.Path)
	require.Equal(t, 3, got.Line)
}

func TestFindCallSiteTaggedFileTooLargeFallsThroughWithANote(t *testing.T) {
	srv := largeFileHost(t, nil, "app/models/order.rb", nil)
	got, err := FindCallSite(context.Background(), githubClient(t, srv, 0), csRepo, 9,
		fileLineTags("app/models/order.rb", "42"), "")
	require.NoError(t, err)
	require.Equal(t, CallSiteNone, got.Confidence)
	require.Contains(t, got.Note, "app/models/order.rb is larger than")
	require.Contains(t, got.Note, "add sqlcommenter tags")
}
