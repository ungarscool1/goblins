package jira

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/ungarscool1/goblins/internal/core"
)

type issue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary     string   `json:"summary"`
		Description *string  `json:"description"`
		Labels      []string `json:"labels"`
		Reporter    *user    `json:"reporter"`
		Created     string   `json:"created"`
		Updated     string   `json:"updated"`
		Project     struct {
			Key string `json:"key"`
		} `json:"project"`
		Status struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
	} `json:"fields"`
	Changelog struct {
		Histories []history `json:"histories"`
	} `json:"changelog"`
}

type history struct {
	ID      string `json:"id"`
	Author  *user  `json:"author"`
	Created string `json:"created"`
	Items   []struct {
		Field      string `json:"field"`
		FromString string `json:"fromString"`
		ToString   string `json:"toString"`
	} `json:"items"`
}

func (i issue) open() bool { return i.Fields.Status.StatusCategory.Key != "done" }

func (i issue) reporter() core.Actor {
	if i.Fields.Reporter == nil {
		return core.Actor{Type: core.ActorUnknown}
	}
	return i.Fields.Reporter.actor()
}

func (i issue) canonical(base string) core.Issue {
	body := ""
	if i.Fields.Description != nil {
		body = *i.Fields.Description
	}
	return core.Issue{Ref: core.IssueRef{Container: i.Fields.Project.Key, ExternalID: i.Key},
		Title: i.Fields.Summary, Body: body, Open: i.open(), Labels: i.Fields.Labels,
		Author: i.reporter(), URL: base + "/browse/" + i.Key,
		CreatedAt: jiraTime(i.Fields.Created), UpdatedAt: jiraTime(i.Fields.Updated)}
}

type comment struct {
	ID         string `json:"id"`
	Author     user   `json:"author"`
	Body       string `json:"body"`
	Created    string `json:"created"`
	Updated    string `json:"updated"`
	Properties []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	} `json:"properties"`
}

func (c comment) idempotencyKey() string {
	for _, p := range c.Properties {
		if p.Key != propertyKey {
			continue
		}
		var v struct {
			Key string `json:"idempotency_key"`
		}
		if json.Unmarshal(p.Value, &v) == nil {
			return v.Key
		}
	}
	return ""
}

// canonical marks a comment as this integration's own only when it carries the property and
// was written by the account Goblins writes as. That account is also a person's, whose own
// comments stay theirs: they can approve, where Goblins' cannot.
func (c comment) canonical(ref core.IssueRef, me user) core.Comment {
	cc := core.Comment{ID: c.ID, Issue: ref, Author: c.Author.actor(), Body: c.Body,
		CreatedAt: jiraTime(c.Created), UpdatedAt: jiraTime(c.Updated)}
	if c.idempotencyKey() != "" && c.Author.id() == me.id() {
		cc.Own, cc.Author.Type = true, core.ActorBot
	}
	return cc
}

// comments walks an issue's comments, oldest first.
func (c *Client) comments(ctx context.Context, key string, fn func(comment) error) error {
	for page, start := 0, 0; page < maxPages; page++ {
		var out struct {
			Total    int       `json:"total"`
			Comments []comment `json:"comments"`
		}
		q := url.Values{"startAt": {strconv.Itoa(start)}, "maxResults": {strconv.Itoa(pageSize)},
			"expand": {"properties"}, "orderBy": {"created"}}
		if err := c.get(ctx, "/rest/api/2/issue/"+key+"/comment", q, &out); err != nil {
			return err
		}
		for _, cm := range out.Comments {
			if err := fn(cm); err != nil {
				return err
			}
		}
		start += len(out.Comments)
		if len(out.Comments) == 0 || start >= out.Total {
			return nil
		}
	}
	return core.Errorf(core.KindUnsupported, "jira.comments", "the issue has too many comments")
}

// search runs a JQL query, in Cloud's token-paged form or Data Center's offset-paged one. It
// stops after limit issues, or the page cap, and says whether it stopped early.
func (c *Client) search(ctx context.Context, jql, expand string, limit int,
	fn func(issue) error) (truncated bool, error error) {
	d, err := c.Deployment(ctx)
	if err != nil {
		return false, err
	}
	seen, token, start := 0, "", 0
	for page := 0; page < maxPages; page++ {
		q := url.Values{"jql": {jql}, "fields": {issueFields},
			"maxResults": {strconv.Itoa(pageSize)}}
		if expand != "" {
			q.Set("expand", expand)
		}
		path := "/rest/api/2/search"
		if d == DeploymentCloud {
			path += "/jql"
			if token != "" {
				q.Set("nextPageToken", token)
			}
		} else {
			q.Set("startAt", strconv.Itoa(start))
		}
		var out struct {
			Issues        []issue `json:"issues"`
			Total         int     `json:"total"`
			NextPageToken string  `json:"nextPageToken"`
		}
		if err := c.get(ctx, path, q, &out); err != nil {
			return false, err
		}
		for _, i := range out.Issues {
			if limit > 0 && seen >= limit {
				return true, nil
			}
			seen++
			if err := fn(i); err != nil {
				return false, err
			}
		}
		start += len(out.Issues)
		token = out.NextPageToken
		last := len(out.Issues) == 0 || (d == DeploymentCloud && token == "") ||
			(d != DeploymentCloud && start >= out.Total)
		if last {
			return false, nil
		}
	}
	return true, nil
}
