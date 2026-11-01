package github

import (
	"fmt"
	"net/url"
	"time"
)

// Commit is the subset of GitHub's commit resource that reports use.
type Commit struct {
	SHA     string    `json:"sha"`
	HTMLURL string    `json:"html_url"`
	Commit  GitCommit `json:"commit"`
	// Author is the GitHub account linked to the git author, or nil if
	// GitHub could not match the commit to an account.
	Author *User `json:"author"`
}

// GitCommit is the git data of a commit.
type GitCommit struct {
	Message   string       `json:"message"`
	Author    GitSignature `json:"author"`
	Committer GitSignature `json:"committer"`
}

// GitSignature is a git author or committer. Email addresses are not
// decoded: reports do not need them, and leaving them out keeps personal
// data out of job results.
type GitSignature struct {
	Name string    `json:"name"`
	Date time.Time `json:"date"`
}

// CommitListOptions filters ListCommits.
type CommitListOptions struct {
	// SHA is the branch name or commit SHA to list from. Empty means the
	// repository's default branch.
	SHA string
	// Since and Until, if set, bound the commit dates returned.
	Since time.Time
	Until time.Time

	ListOptions
}

// ListCommits returns a Pager over a repository's commits, newest first.
//
// An empty repository has no commits to list, and GitHub answers with
// 409 Conflict, returned as an *APIError.
//
// No request is made until the first call to NextPage or All.
func (c *Client) ListCommits(owner, repo string, opts CommitListOptions) (*Pager[Commit], error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	if !opts.Since.IsZero() && !opts.Until.IsZero() && opts.Until.Before(opts.Since) {
		return nil, fmt.Errorf("%w: until (%s) is before since (%s)",
			ErrInvalidArgument, formatTime(opts.Until), formatTime(opts.Since))
	}
	q := url.Values{}
	if opts.SHA != "" {
		q.Set("sha", opts.SHA)
	}
	if !opts.Since.IsZero() {
		q.Set("since", formatTime(opts.Since))
	}
	if !opts.Until.IsZero() {
		q.Set("until", formatTime(opts.Until))
	}
	return newPager(c, []string{"repos", owner, repo, "commits"}, q, opts.ListOptions, decodeList[Commit])
}
