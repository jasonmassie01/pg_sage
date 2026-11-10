package docscontract

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// A stand-in for `mkdocs build --strict` link validation, which CI runs
// on every docs pull request: these helpers find the relative links and
// heading anchors the way Python-Markdown's toc extension renders them.

var (
	inlineLink   = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	refLinkDef   = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*<?(\S+?)>?(?:\s|$)`)
	codeSpan     = regexp.MustCompile("`+[^`]*`+")
	fenceLine    = regexp.MustCompile("^\\s*(```|~~~)")
	atxHeading   = regexp.MustCompile(`^\s{0,12}(#{1,6})\s+(.*?)\s*#*\s*$`)
	attrListID   = regexp.MustCompile(`\s*\{[^}]*#([A-Za-z][\w-]*)[^}]*\}\s*$`)
	htmlID       = regexp.MustCompile(`\b(?:id|name)\s*=\s*["']([^"']+)["']`)
	mdLinkText   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	htmlTag      = regexp.MustCompile(`<[^>]+>`)
	slugDrop     = regexp.MustCompile(`[^\w\s-]`)
	slugSpace    = regexp.MustCompile(`[-\s]+`)
	urlWithProto = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// proseLines returns the lines of a Markdown body outside fenced code.
func proseLines(body string) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if fenceLine.MatchString(line) {
			inFence = !inFence
			continue
		}
		if !inFence {
			out = append(out, line)
		}
	}
	return out
}

// linkTargets returns every link target in prose: inline links, images
// and reference definitions, ignoring links inside code.
func linkTargets(body string) []string {
	var out []string
	for _, line := range proseLines(body) {
		line = codeSpan.ReplaceAllString(line, "")
		if m := refLinkDef.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
			continue
		}
		for _, m := range inlineLink.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// slugify is Python-Markdown's toc slugify for ASCII text: drop what is
// not a word character, space or hyphen, lowercase, and join words with
// hyphens. Non-ASCII runes are dropped, as its NFKD-to-ASCII step drops
// the dashes and arrows these pages use.
func slugify(text string) string {
	var b strings.Builder
	for _, r := range text {
		if r < 128 {
			b.WriteRune(r)
		}
	}
	s := strings.ToLower(strings.TrimSpace(slugDrop.ReplaceAllString(b.String(), "")))
	return slugSpace.ReplaceAllString(s, "-")
}

// headingText renders a heading's inline Markdown to its plain text.
func headingText(raw string) string {
	s := mdLinkText.ReplaceAllString(raw, "$1")
	s = htmlTag.ReplaceAllString(s, "")
	s = strings.NewReplacer("`", "", "**", "", "&amp;", "&").Replace(s)
	return strings.TrimSpace(s)
}

// pageAnchors returns the ids a rendered page carries: one per heading
// (an attr_list id when given, else the slug, suffixed _1, _2 on repeats
// as toc does) plus explicit HTML id and name attributes.
func pageAnchors(body string) map[string]bool {
	ids := map[string]bool{}
	for _, line := range proseLines(body) {
		for _, m := range htmlID.FindAllStringSubmatch(line, -1) {
			ids[m[1]] = true
		}
		h := atxHeading.FindStringSubmatch(line)
		if h == nil {
			continue
		}
		if m := attrListID.FindStringSubmatch(h[2]); m != nil {
			ids[m[1]] = true
			continue
		}
		id := slugify(headingText(h[2]))
		unique := id
		for n := 1; ids[unique]; n++ {
			unique = fmt.Sprintf("%s_%d", id, n)
		}
		ids[unique] = true
	}
	return ids
}

// linkProblem checks one link target of page against the published set,
// returning "" when MkDocs would resolve it. published maps docs-relative
// paths of pages to their bodies; other files are checked on disk.
func linkProblem(docs, page, target string, published map[string]string) string {
	if urlWithProto.MatchString(target) || strings.HasPrefix(target, "/") {
		return ""
	}
	file, anchor, _ := strings.Cut(target, "#")
	if q := strings.IndexByte(file, '?'); q >= 0 {
		file = file[:q]
	}
	dest := page
	if file != "" {
		dest = path.Clean(path.Join(path.Dir(page), file))
	}
	if dest == ".." || strings.HasPrefix(dest, "../") {
		return fmt.Sprintf("%s links to %s, outside docs/; use a GitHub URL", page, target)
	}
	body, isPage := published[dest]
	if !isPage {
		if strings.HasSuffix(dest, ".md") {
			return fmt.Sprintf("%s links to %s, which is not a published page", page, target)
		}
		if info, err := os.Stat(filepath.Join(docs, filepath.FromSlash(dest))); err != nil ||
			info.IsDir() {
			return fmt.Sprintf("%s links to %s, which is not a file under docs/", page, target)
		}
		return ""
	}
	if anchor != "" && !pageAnchors(body)[anchor] {
		return fmt.Sprintf("%s links to %s, but %s has no anchor #%s", page, target, dest, anchor)
	}
	return ""
}
