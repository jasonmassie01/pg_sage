package briefing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-06: Slack gets mrkdwn, not raw markdown, and model text cannot ping.

// No concurrent access tests: toSlackMrkdwn is a pure function.

func TestToSlackMrkdwnMapsMarkdown(t *testing.T) {
	tests := []struct{ in, want string }{
		{"**x**", "*x*"},
		{"# H", "*H*"},
		{"## Findings: 1 critical", "*Findings: 1 critical*"},
		{"### deep", "*deep*"},
		{"- **db_size**: 1 GB", "- *db_size*: 1 GB"},
		{"plain text", "plain text"},
		{"#hashtag stays", "#hashtag stays"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := toSlackMrkdwn(tt.in); got != tt.want {
			t.Errorf("toSlackMrkdwn(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Invalid input: control sequences from the model or the database are
// escaped, so they render as text instead of pinging or linking.
func TestToSlackMrkdwnEscapesControlSequences(t *testing.T) {
	got := toSlackMrkdwn("# Alert <!channel>\nsee <https://evil.example|here> & more")
	for _, bad := range []string{"<!channel>", "<https://", "|here>"} {
		if strings.Contains(got, bad) {
			t.Fatalf("control sequence %q survived: %q", bad, got)
		}
	}
	want := "*Alert &lt;!channel&gt;*\nsee &lt;https://evil.example|here&gt; &amp; more"
	if got != want {
		t.Fatalf("toSlackMrkdwn = %q, want %q", got, want)
	}
}

// Boundary: a briefing over Slack's limit is cut on a rune boundary.
func TestToSlackMrkdwnTruncates(t *testing.T) {
	long := strings.Repeat("é", slackMaxRunes+50)
	got := toSlackMrkdwn(long)
	if n := utf8.RuneCountInString(got); n != slackMaxRunes {
		t.Fatalf("rune count = %d, want %d", n, slackMaxRunes)
	}
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatal("truncated text is not valid UTF-8 ending in an ellipsis")
	}
}

// Happy path end to end: the posted text is mrkdwn.
func TestSendSlackPostsMrkdwn(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got.Store(body["text"])
	}))
	defer srv.Close()
	w := &Worker{cfg: &config.Config{Briefing: config.BriefingConfig{
		SlackWebhookURL: srv.URL}}, logFn: noopLog}
	d := w.sendSlack(context.Background(), "# Daily\n**bold** <!here>")
	if d.Status != DeliverySent || d.HTTPStatus != http.StatusOK {
		t.Fatalf("delivery = %+v, want sent with HTTP 200", d)
	}
	if text, _ := got.Load().(string); text != "*Daily*\n*bold* &lt;!here&gt;" {
		t.Fatalf("posted text = %q", text)
	}
}
