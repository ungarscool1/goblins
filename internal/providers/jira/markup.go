package jira

import (
	"regexp"
	"strings"
)

var (
	mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdFence   = regexp.MustCompile("^(`{3,}|~{3,})\\s*([A-Za-z0-9_+-]*)\\s*$")
	mdBullet  = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	mdNumber  = regexp.MustCompile(`^(\s*)\d+[.)]\s+(.*)$`)
	mdRule    = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
	mdCode    = regexp.MustCompile("`([^`\n]+)`")
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdLink    = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s]+)\)`)
	// What Jira's markup would act on in a way that matters: macros, links and mentions,
	// tables, and images, which would load from anywhere. Effects such as -strike- are left.
	wikiSpecial = regexp.MustCompile(`[{}\[\]|!]`)
)

// ToWiki converts the Markdown Goblins writes into Jira's wiki markup. Everything that is not
// formatting Goblins itself produced is escaped, so text from a ticket, a model or a test
// cannot mention someone, open a macro or break out of a code block.
func ToWiki(md string) string {
	var out []string
	fence := ""
	for _, line := range strings.Split(md, "\n") {
		if fence != "" {
			if strings.TrimSpace(line) == fence {
				fence = ""
				out = append(out, "{code}")
				continue
			}
			// A code block only ends at {code}, which it must not contain.
			out = append(out, strings.ReplaceAll(line, "{code", "{ code"))
			continue
		}
		if m := mdFence.FindStringSubmatch(line); m != nil {
			fence = m[1]
			out = append(out, "{code}")
			continue
		}
		out = append(out, wikiLine(line))
	}
	if fence != "" {
		out = append(out, "{code}")
	}
	return strings.Join(out, "\n")
}

func wikiLine(line string) string {
	if m := mdHeading.FindStringSubmatch(line); m != nil {
		return "h" + string(rune('0'+len(m[1]))) + ". " + inline(m[2])
	}
	if mdRule.MatchString(line) {
		return "----"
	}
	if m := mdBullet.FindStringSubmatch(line); m != nil {
		return strings.Repeat("*", depth(m[1])) + " " + inline(m[2])
	}
	if m := mdNumber.FindStringSubmatch(line); m != nil {
		return strings.Repeat("#", depth(m[1])) + " " + inline(m[2])
	}
	return inline(line)
}

func depth(indent string) int {
	return 1 + len(strings.ReplaceAll(indent, "\t", "  "))/2
}

// inline converts code, bold and links, and escapes everything else. Code spans are cut out
// first, so nothing inside them is formatted.
func inline(s string) string {
	var b strings.Builder
	for {
		loc := mdCode.FindStringSubmatchIndex(s)
		if loc == nil {
			b.WriteString(text(s))
			return b.String()
		}
		b.WriteString(text(s[:loc[0]]))
		code := s[loc[2]:loc[3]]
		b.WriteString("{{" + wikiSpecial.ReplaceAllString(code, `\$0`) + "}}")
		s = s[loc[1]:]
	}
}

func text(s string) string {
	type span struct {
		start, end int
		out        string
	}
	var spans []span
	for _, m := range mdLink.FindAllStringSubmatchIndex(s, -1) {
		label := escape(s[m[2]:m[3]])
		link := strings.NewReplacer("|", "%7C", "]", "%5D", "[", "%5B").Replace(s[m[4]:m[5]])
		spans = append(spans, span{m[0], m[1], "[" + label + "|" + link + "]"})
	}
	var b strings.Builder
	at := 0
	for _, sp := range spans {
		b.WriteString(bold(s[at:sp.start]))
		b.WriteString(sp.out)
		at = sp.end
	}
	b.WriteString(bold(s[at:]))
	return b.String()
}

func bold(s string) string {
	var b strings.Builder
	at := 0
	for _, m := range mdBold.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(escape(strings.ReplaceAll(s[at:m[0]], "*", `\*`)))
		b.WriteString("*" + escape(s[m[2]:m[3]]) + "*")
		at = m[1]
	}
	b.WriteString(escape(strings.ReplaceAll(s[at:], "*", `\*`)))
	return b.String()
}

func escape(s string) string {
	return wikiSpecial.ReplaceAllString(s, `\$0`)
}
