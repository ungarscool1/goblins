package knowledge

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

// Outline searches Outline documents in an allow-list of collections, with an API token that
// should itself be read-only.
type Outline struct {
	name        string
	c           *client
	collections []string
}

// NewOutline returns a source.
func NewOutline(name, base string, token core.Secret, collections []string) (*Outline, error) {
	if len(collections) == 0 {
		return nil, core.Errorf(core.KindInvalid, "knowledge.outline",
			"collections allow-list is required")
	}
	c, err := newClient("knowledge.outline", base, "Bearer "+token.Reveal())
	if err != nil {
		return nil, err
	}
	return &Outline{name: name, c: c, collections: collections}, nil
}

// Info implements core.Provider.
func (o *Outline) Info() core.ProviderInfo { return info(o.name, "outline") }

// Search implements core.KnowledgeSource, one collection at a time, so a document outside the
// allow-list is never even returned.
func (o *Outline) Search(ctx context.Context, q core.KnowledgeQuery) ([]core.KnowledgeDoc, error) {
	terms := words(q.Terms)
	if len(terms) == 0 {
		return nil, nil
	}
	var out []core.KnowledgeDoc
	for _, col := range o.collections {
		var res struct {
			Data []struct {
				Document struct {
					ID           string `json:"id"`
					Title        string `json:"title"`
					Text         string `json:"text"`
					URL          string `json:"url"`
					UpdatedAt    string `json:"updatedAt"`
					CollectionID string `json:"collectionId"`
					Revision     int    `json:"revision"`
				} `json:"document"`
			} `json:"data"`
		}
		in := map[string]any{"query": strings.Join(terms, " "), "collectionId": col,
			"limit": max(q.Limit, 1), "offset": 0}
		if err := o.c.do(ctx, http.MethodPost, "/api/documents.search", in, &res); err != nil {
			return nil, err
		}
		for _, d := range res.Data {
			doc := d.Document
			if !slices.Contains(o.collections, doc.CollectionID) {
				continue
			}
			u := doc.URL
			if strings.HasPrefix(u, "/") {
				u = o.c.base + u
			}
			out = append(out, core.KnowledgeDoc{ID: doc.ID, Title: doc.Title, Body: doc.Text,
				URL: u, UpdatedAt: since(doc.UpdatedAt), Revision: plural(doc.Revision, "edits")})
		}
	}
	return out, nil
}
