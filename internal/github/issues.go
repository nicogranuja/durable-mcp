package github

import (
	"encoding/json"
	"net/url"
	"time"
)

// State filters issues and pull requests by state.
type State string

// States accepted by the list methods. GitHub defaults to StateOpen.
const (
	StateOpen   State = "open"
	StateClosed State = "closed"
	StateAll    State = "all"
)

// Label is an issue or pull request label.
type Label struct {
	Name string `json:"name"`
}

// Issue is the subset of GitHub's issue resource that reports use.
type Issue struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	// State is "open" or "closed".
	State string `json:"state"`
	// StateReason says why an issue is in its state, such as "completed"
	// or "not_planned" for a closed issue. It is often empty.
	StateReason string  `json:"state_reason"`
	User        User    `json:"user"`
	Labels      []Label `json:"labels"`
	Comments    int     `json:"comments"`
	HTMLURL     string  `json:"html_url"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ClosedAt is nil for open issues.
	ClosedAt *time.Time `json:"closed_at"`
}

// IssueListOptions filters ListIssues.
type IssueListOptions struct {
	// State filters by state. Empty means GitHub's default, StateOpen.
	State State
	// Since, if set, limits results to issues updated at or after it.
	Since time.Time

	ListOptions
}

// ListIssues returns a Pager over a repository's issues, excluding pull
// requests.
//
// GitHub's issues endpoint returns pull requests as well, marked with a
// pull_request field. ListIssues drops them, so a page can hold fewer items
// than the page size; use ListPullRequests for pull requests.
//
// No request is made until the first call to NextPage or All.
func (c *Client) ListIssues(owner, repo string, opts IssueListOptions) (*Pager[Issue], error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", string(opts.State))
	}
	if !opts.Since.IsZero() {
		q.Set("since", formatTime(opts.Since))
	}
	return newPager(c, []string{"repos", owner, repo, "issues"}, q, opts.ListOptions, decodeIssues)
}

func decodeIssues(body []byte) ([]Issue, error) {
	var page []struct {
		Issue
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(page))
	for _, item := range page {
		if item.PullRequest == nil {
			issues = append(issues, item.Issue)
		}
	}
	return issues, nil
}

// formatTime formats t as the ISO 8601 timestamp GitHub expects in query
// parameters.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
