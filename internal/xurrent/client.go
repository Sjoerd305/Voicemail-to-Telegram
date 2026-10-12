// Package xurrent checks whether a voicemail is followed by activity in
// Xurrent (https://developer.xurrent.com/v1/): a new request or a note
// written by one of the tracked teams.
package xurrent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Client is a minimal read-only Xurrent REST client.
type Client struct {
	baseURL string
	account string
	token   string
	http    *http.Client
}

func NewClient(baseURL, account, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		account: account,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type Ref struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
}

type Note struct {
	ID        int       `json:"id"`
	Person    Ref       `json:"person"`
	CreatedAt time.Time `json:"created_at"`
	Text      string    `json:"text"`
	Medium    string    `json:"medium"`
}

type Request struct {
	ID        int       `json:"id"`
	Subject   string    `json:"subject"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy *Ref      `json:"created_by"`
}

// FindTeam returns the team with exactly this name.
func (c *Client) FindTeam(ctx context.Context, name string) (Ref, error) {
	var teams []Ref
	if err := c.list(ctx, "/teams", url.Values{"name": {name}}, &teams); err != nil {
		return Ref{}, err
	}
	for _, t := range teams {
		if t.Name == name {
			return t, nil
		}
	}
	return Ref{}, fmt.Errorf("xurrent team %q not found", name)
}

// FindPerson returns the person with exactly this name. A name that is not
// unique is an error: guessing could track the wrong person.
func (c *Client) FindPerson(ctx context.Context, name string) (Ref, error) {
	var people []Ref
	if err := c.list(ctx, "/people", url.Values{"name": {name}}, &people); err != nil {
		return Ref{}, err
	}
	var found []Ref
	for _, p := range people {
		// A disabled account cannot write notes; an old one with the same
		// name must not make the lookup ambiguous.
		if p.Name == name && !p.Disabled {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return Ref{}, fmt.Errorf("xurrent person %q not found", name)
	case 1:
		return found[0], nil
	default:
		return Ref{}, fmt.Errorf("xurrent person name %q is not unique (ids %v); use person_ids", name, refIDs(found))
	}
}

func refIDs(refs []Ref) []int {
	ids := make([]int, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids
}

func (c *Client) TeamMembers(ctx context.Context, teamID int) ([]Ref, error) {
	var people []Ref
	err := c.list(ctx, fmt.Sprintf("/teams/%d/members", teamID), nil, &people)
	return people, err
}

// RequestNotes lists the notes these people typed on one request in
// [from, to). Inbound mail, outbound mail (the automatic acknowledgements
// are sent in a colleague's name), system and automation notes don't count.
//
// The account-wide /notes listing is not used: it only returns notes of the
// account in the X-Xurrent-Account header, while the requests live in a
// trusted account (the service portal), so it silently misses most notes.
func (c *Client) RequestNotes(ctx context.Context, requestID int, personIDs []int, from, to time.Time) ([]Note, error) {
	var notes []Note
	err := c.list(ctx, fmt.Sprintf("/requests/%d/notes", requestID), url.Values{
		"person":     {joinIDs(personIDs)},
		"created_at": {timeRange(from, to)},
		"medium":     {"default"},
	}, &notes)
	return notes, err
}

// requestFields are the request fields the checks need.
const requestFields = "id,subject,created_at,updated_at,created_by"

// RequestsCreatedBy lists requests these people created in [from, to),
// on team teamID when that is not 0.
func (c *Client) RequestsCreatedBy(ctx context.Context, personIDs []int, teamID int, from, to time.Time) ([]Request, error) {
	q := url.Values{
		"created_by": {joinIDs(personIDs)},
		"created_at": {timeRange(from, to)},
		"fields":     {requestFields},
	}
	if teamID != 0 {
		q.Set("team", strconv.Itoa(teamID))
	}
	var reqs []Request
	err := c.list(ctx, "/requests", q, &reqs)
	return reqs, err
}

// RequestsUpdatedSince lists every request (any team) updated at or after
// from. A note added at time t leaves updated_at >= t, so these are all the
// requests that can hold a note from t on.
func (c *Client) RequestsUpdatedSince(ctx context.Context, from time.Time) ([]Request, error) {
	var reqs []Request
	err := c.list(ctx, "/requests", url.Values{
		"updated_at": {">=" + from.UTC().Format(time.RFC3339)},
		"fields":     {requestFields},
	}, &reqs)
	return reqs, err
}

func timeRange(from, to time.Time) string {
	return ">=" + from.UTC().Format(time.RFC3339) + "<" + to.UTC().Format(time.RFC3339)
}

func joinIDs(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

// maxPages bounds a single listing (100 items per page); a month of
// updated requests for a backtest is well below it.
const maxPages = 100

// list fetches every page of a collection and appends the items to out,
// which must point to a slice.
func (c *Client) list(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("per_page", "100")
	next := c.baseURL + path + "?" + q.Encode()
	var all []json.RawMessage
	for page := 0; next != ""; page++ {
		if page == maxPages {
			return fmt.Errorf("xurrent %s: more than %d pages", path, maxPages)
		}
		body, link, err := c.get(ctx, next)
		if err != nil {
			return err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return fmt.Errorf("xurrent %s: decode: %w", path, err)
		}
		all = append(all, items...)
		next = nextLink(link)
	}
	joined, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(joined, out)
}

// maxRetries is how often a rate-limited (429) call is retried.
const maxRetries = 3

func (c *Client) get(ctx context.Context, u string) ([]byte, string, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("X-Xurrent-Account", c.account)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("xurrent: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, "", fmt.Errorf("xurrent: read body: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
			wait := 10 * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				wait = time.Duration(s) * time.Second
			}
			if wait > time.Minute {
				wait = time.Minute
			}
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("xurrent %s: %s: %s", redactQuery(u), resp.Status, strings.TrimSpace(string(body)))
		}
		return body, resp.Header.Get("Link"), nil
	}
}

var nextRe = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

func nextLink(link string) string {
	if m := nextRe.FindStringSubmatch(link); m != nil {
		return m[1]
	}
	return ""
}

// redactQuery keeps error messages short; the query only holds filters.
func redactQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
