package knowledge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ungarscool1/goblins/internal/core"
)

var spaceKey = regexp.MustCompile(`^[A-Za-z0-9~_]{1,255}$`)

// Confluence searches Confluence Cloud pages in an allow-list of spaces, as the account whose
// credentials it is given.
type Confluence struct {
	name   string
	c      *client
	spaces []string
}

// NewConfluence returns a source. auth is a ready Authorization header value.
func NewConfluence(name, base, auth string, spaces []string) (*Confluence, error) {
	if len(spaces) == 0 {
		return nil, core.Errorf(core.KindInvalid, "knowledge.confluence",
			"spaces allow-list is required")
	}
	for _, s := range spaces {
		if !spaceKey.MatchString(s) {
			return nil, core.Errorf(core.KindInvalid, "knowledge.confluence",
				"%q is not a space key", s)
		}
	}
	c, err := newClient("knowledge.confluence", base, auth)
	if err != nil {
		return nil, err
	}
	return &Confluence{name: name, c: c, spaces: spaces}, nil
}

// Info implements core.Provider.
func (f *Confluence) Info() core.ProviderInfo { return info(f.name, "confluence") }

// Search implements core.KnowledgeSource with CQL built only from validated space keys and
// plain words.
func (f *Confluence) Search(ctx context.Context, q core.KnowledgeQuery) ([]core.KnowledgeDoc,
	error) {
	terms := words(q.Terms)
	if len(terms) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(f.spaces))
	for i, s := range f.spaces {
		quoted[i] = `"` + s + `"`
	}
	cql := fmt.Sprintf(`type = page AND space IN (%s) AND text ~ "%s"`,
		strings.Join(quoted, ","), strings.Join(terms, " "))
	v := url.Values{"cql": {cql}, "limit": {strconv.Itoa(max(q.Limit, 1))},
		"expand": {"body.storage,version,space"}}
	var res struct {
		Results []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Space struct {
				Key string `json:"key"`
			} `json:"space"`
			Version struct {
				Number int    `json:"number"`
				When   string `json:"when"`
			} `json:"version"`
			Body struct {
				Storage struct {
					Value string `json:"value"`
				} `json:"storage"`
			} `json:"body"`
			Links struct {
				WebUI string `json:"webui"`
			} `json:"_links"`
		} `json:"results"`
		Links struct {
			Base string `json:"base"`
		} `json:"_links"`
	}
	if err := f.c.do(ctx, http.MethodGet, "/wiki/rest/api/content/search?"+v.Encode(), nil,
		&res); err != nil {
		return nil, err
	}
	var out []core.KnowledgeDoc
	for _, r := range res.Results {
		if !slices.Contains(f.spaces, r.Space.Key) {
			continue
		}
		out = append(out, core.KnowledgeDoc{ID: r.ID, Title: r.Title,
			Body: plain(r.Body.Storage.Value), URL: res.Links.Base + r.Links.WebUI,
			Revision: "v" + strconv.Itoa(r.Version.Number), UpdatedAt: since(r.Version.When)})
	}
	return out, nil
}
