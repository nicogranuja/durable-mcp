package github

import (
	"fmt"
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

// PullRequestSort orders ListPullRequests results.
type PullRequestSort string

// Orders accepted by ListPullRequests. GitHub defaults to SortCreated.
const (
	SortCreated     PullRequestSort = "created"
	SortUpdated     PullRequestSort = "updated"
	SortPopularity  PullRequestSort = "popularity"
	SortLongRunning PullRequestSort = "long-running"
)

// Direction is a sort direction.
type Direction string

// Sort directions.
const (
	Ascending  Direction = "asc"
	Descending Direction = "desc"
)

// PullRequestListOptions filters and orders ListPullRequests.
type PullRequestListOptions struct {
	// State filters by state. Empty means GitHub's default, StateOpen.
	State State
	// Sort orders the results. Empty means GitHub's default, SortCreated.
	Sort PullRequestSort
	// Direction is the sort direction. Empty means GitHub's default:
	// Descending for SortCreated, Ascending otherwise.
	Direction Direction

	ListOptions
}

// ListPullRequests returns a Pager over a repository's pull requests.
//
// The endpoint has no "since" filter. To list recent activity, use
// SortUpdated with Descending and stop iterating once UpdatedAt falls
// outside the window.
//
// No request is made until the first call to NextPage or All.
func (c *Client) ListPullRequests(owner, repo string, opts PullRequestListOptions) (*Pager[PullRequest], error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	if err := opts.State.validate(); err != nil {
		return nil, err
	}
	switch opts.Sort {
	case "", SortCreated, SortUpdated, SortPopularity, SortLongRunning:
	default:
		return nil, fmt.Errorf("%w: unknown pull request sort %q", ErrInvalidArgument, string(opts.Sort))
	}
	switch opts.Direction {
	case "", Ascending, Descending:
	default:
		return nil, fmt.Errorf("%w: unknown sort direction %q", ErrInvalidArgument, string(opts.Direction))
	}

	q := url.Values{}
	if opts.State != "" {
		q.Set("state", string(opts.State))
	}
	if opts.Sort != "" {
		q.Set("sort", string(opts.Sort))
	}
	if opts.Direction != "" {
		q.Set("direction", string(opts.Direction))
	}
	return newPager(c, owner, repo, "pulls", q, opts.ListOptions, decodeList[PullRequest])
}
