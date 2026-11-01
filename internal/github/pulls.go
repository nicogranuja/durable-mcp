package github

import (
	"net/url"
	"time"
)

// PullRequest is the subset of GitHub's pull request resource that reports
// use.
type PullRequest struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	// State is "open" or "closed". A merged pull request is closed with a
	// non-nil MergedAt.
	State   string  `json:"state"`
	Draft   bool    `json:"draft"`
	User    User    `json:"user"`
	Labels  []Label `json:"labels"`
	HTMLURL string  `json:"html_url"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedAt  *time.Time `json:"merged_at"`
}

// PullRequestListOptions filters and orders ListPullRequests.
type PullRequestListOptions struct {
	// State filters by state. Empty means GitHub's default, StateOpen.
	State State
	// Sort is "created" (the default), "updated", "popularity", or
	// "long-running".
	Sort string
	// Direction is "asc" or "desc". GitHub defaults to "desc" when Sort is
	// empty or "created", and to "asc" otherwise.
	Direction string

	ListOptions
}

// ListPullRequests returns a Pager over a repository's pull requests.
//
// The endpoint has no "since" filter. To list recent activity, sort by
// "updated" in "desc" order and stop iterating once UpdatedAt falls outside
// the window.
//
// No request is made until the first call to NextPage or All.
func (c *Client) ListPullRequests(owner, repo string, opts PullRequestListOptions) (*Pager[PullRequest], error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.State != "" {
		q.Set("state", string(opts.State))
	}
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	if opts.Direction != "" {
		q.Set("direction", opts.Direction)
	}
	return newPager(c, owner, repo, "pulls", q, opts.ListOptions, decodeList[PullRequest])
}
