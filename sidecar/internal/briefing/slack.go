package briefing

import (
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/notify"
)

// slackMaxRunes keeps a briefing under Slack's 40,000-character text limit.
const slackMaxRunes = 39000

var (
	slackHeadingRE = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*$`)
	slackBoldRE    = regexp.MustCompile(`\*\*(.+?)\*\*`)
)

// toSlackMrkdwn renders briefing markdown as Slack mrkdwn (llm-06). Text
// is escaped first, so <!channel> or a <url|label> link from the model or
// the database renders as text; headings and **bold** become *bold*.
func toSlackMrkdwn(text string) string {
	out := notify.EscapeMrkdwn(text)
	out = slackHeadingRE.ReplaceAllStringFunc(out, func(line string) string {
		title := slackHeadingRE.FindStringSubmatch(line)[1]
		return "*" + strings.ReplaceAll(title, "*", "") + "*"
	})
	out = slackBoldRE.ReplaceAllString(out, "*$1*")
	return notify.TruncateRunes(out, slackMaxRunes)
}
