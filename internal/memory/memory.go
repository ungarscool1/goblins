// Package memory builds what agents are told about a project beyond the ticket: accepted
// memories Goblins keeps, and documents from knowledge sources people maintain. Selection is
// deterministic and lexical, needs no model or index, and stays within a project's namespaces.
// Everything it returns is cited, bounded, and data to the agent, never instructions.
package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ungarscool1/goblins/internal/core"
)

// Defaults bound a pack. An item longer than maxItem is cut, and a pack stops at its budget.
const (
	DefaultBudget = 8 << 10
	maxItem       = 1500
	maxTerms      = 12
	perSource     = 5
)

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for that this with from have has are was were will
		would should could can not but any all into onto when what which while where who how why
		its our your their them they you about there here then than been being also only just more
		most some such use using used make made need needs please add adds added fix fixes issue
		ticket goblins should must does did done get got new one two out over under like want`) {
		stopwords[w] = true
	}
}

// Terms extracts search words from text: lower case letters and digits, three or more, without
// common words, most frequent first. Only these reach a source's query language.
func Terms(text string) []string {
	counts := map[string]int{}
	var order []string
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)))
	}) {
		if len(w) < 3 || len(w) > 40 || stopwords[w] {
			continue
		}
		if counts[w] == 0 {
			order = append(order, w)
		}
		counts[w]++
	}
	slices.SortStableFunc(order, func(a, b string) int { return counts[b] - counts[a] })
	return order[:min(len(order), maxTerms)]
}

// Score counts query terms in a title, which weighs three times, and a body.
func Score(title, body string, terms []string) int {
	t, b := strings.ToLower(title), strings.ToLower(body)
	n := 0
	for _, term := range terms {
		n += 3*strings.Count(t, term) + min(strings.Count(b, term), 10)
	}
	return n
}

// Source is a configured knowledge source. A required one that fails stops the run; an
// optional one is noted and skipped.
type Source struct {
	Name     string
	Source   core.KnowledgeSource
	Required bool
}

// Request says what to look for, and where.
type Request struct {
	Namespaces []string
	Store      core.MemoryStore
	Sources    []Source
	Text       string
	Budget     int
	Now        time.Time
}

// Item is one entry of a pack, with what it cites.
type Item struct {
	Ref      string
	Citation string
	Title    string
	Body     string
	score    int
}

// Pack is what an agent is given, and the record of it.
type Pack struct {
	Items    []Item
	Warnings []string
}

// Build selects the most relevant accepted memories and documents within the budget.
func Build(ctx context.Context, r Request) (Pack, error) {
	terms := Terms(r.Text)
	var pack Pack
	if len(terms) == 0 {
		return pack, nil
	}
	items, err := memories(ctx, r, terms)
	if err != nil {
		return pack, err
	}
	for _, s := range r.Sources {
		docs, err := s.Source.Search(ctx, core.KnowledgeQuery{Terms: terms, Limit: perSource})
		if err != nil {
			if s.Required {
				return pack, core.Errorf(core.KindOf(err), "memory.source",
					"required knowledge source %s is unavailable: %s", s.Name,
					core.UserMessage(err))
			}
			pack.Warnings = append(pack.Warnings, fmt.Sprintf("knowledge source %s was skipped: %s",
				s.Name, core.UserMessage(err)))
			continue
		}
		for _, d := range docs {
			items = append(items, docItem(s.Name, d, terms))
		}
	}
	slices.SortStableFunc(items, func(a, b Item) int {
		return cmp.Or(b.score-a.score, strings.Compare(a.Ref, b.Ref))
	})
	budget := cmp.Or(r.Budget, DefaultBudget)
	seen := map[string]bool{}
	for _, it := range items {
		key := strings.ToLower(strings.TrimSpace(it.Title))
		if it.score == 0 || seen[key] {
			continue
		}
		overhead := len(it.Ref) + len(it.Citation) + len(it.Title) + 16
		if overhead+200 > budget {
			continue
		}
		if room := budget - overhead; len(it.Body) > room {
			it.Body = strings.ToValidUTF8(it.Body[:room-20], "") + "\n[... cut ...]"
		}
		size := overhead + len(it.Body)
		budget -= size
		seen[key] = true
		pack.Items = append(pack.Items, it)
	}
	return pack, nil
}

func memories(ctx context.Context, r Request, terms []string) ([]Item, error) {
	if r.Store == nil {
		return nil, nil
	}
	all, err := r.Store.ListMemories(ctx, r.Namespaces)
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, m := range all {
		if m.Status != core.MemoryAccepted || (!m.ExpiresAt.IsZero() &&
			!r.Now.Before(m.ExpiresAt)) ||
			!slices.Contains(r.Namespaces, m.Namespace) {
			continue
		}
		verified := "never verified"
		if !m.VerifiedAt.IsZero() {
			verified = "verified " + m.VerifiedAt.UTC().Format("2006-01-02")
		}
		cite := fmt.Sprintf("memory %s, %s, accepted by %s, %s", m.ID, m.Kind, m.ApprovedBy,
			verified)
		if len(m.Sources) > 0 {
			cite += ", from " + strings.Join(m.Sources, ", ")
		}
		out = append(out, Item{Ref: "memory:" + m.ID, Citation: cite, Title: m.Title,
			Body: cut(m.Body), score: Score(m.Title, m.Body+" "+strings.Join(m.Tags, " "), terms)})
	}
	return out, nil
}

func docItem(source string, d core.KnowledgeDoc, terms []string) Item {
	cite := "document from " + source
	if d.Revision != "" {
		cite += ", revision " + d.Revision
	}
	if !d.UpdatedAt.IsZero() {
		cite += ", updated " + d.UpdatedAt.UTC().Format("2006-01-02")
	}
	if d.URL != "" {
		cite += ", " + d.URL
	}
	return Item{Ref: source + ":" + d.ID, Citation: cite, Title: d.Title, Body: cut(d.Body),
		score: Score(d.Title, d.Body, terms)}
}

func cut(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	if len(s) <= maxItem {
		return s
	}
	return strings.ToValidUTF8(s[:maxItem], "") + "\n[... cut ...]"
}

// Text renders a pack for a prompt. It says what the pack is worth: background that can be
// out of date, which the ticket, the approved design and the code override.
func (p Pack) Text() string {
	if len(p.Items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Project memory and documentation, selected for this work. It is background " +
		"that may be out of date: where it disagrees with the ticket, the approved design or the " +
		"code, those win. Cite an entry by its reference when you rely on it.\n")
	for _, it := range p.Items {
		fmt.Fprintf(&b, "\n[%s] %s (%s)\n%s\n", it.Ref, it.Title, it.Citation, it.Body)
	}
	return b.String()
}

// Refs lists what a pack holds, for the record of a run.
func (p Pack) Refs() []string {
	out := make([]string, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Ref
	}
	return out
}
