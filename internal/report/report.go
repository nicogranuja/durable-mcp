package report

import (
	"errors"
	"fmt"
	"time"
)

// The workflow's decisions depend on these values, so changing one while
// reports are running can make their replays diverge from their histories.
// Guard such a change with workflow.GetVersion.
const (
	// DefaultWindow is how far back a report looks when the request does not
	// set Since.
	DefaultWindow = 30 * 24 * time.Hour

	// MaxWindow is the longest window a report may cover. It bounds the
	// per-week commit counts that every commits page carries.
	MaxWindow = 366 * 24 * time.Hour

	// DefaultMaxPages is how many pages of each resource a report fetches
	// when the request does not set MaxPages. At 100 items per page that is
	// 10,000 issues, pull requests, or commits.
	DefaultMaxPages = 100

	// MaxPagesLimit is the largest MaxPages a request may ask for. Every page
	// adds a fixed handful of events to the workflow history, so this caps
	// the history of one report; see the package documentation.
	MaxPagesLimit = 200

	// TopContributors is how many contributors a report lists.
	TopContributors = 10
)

// Request is the input of the RepositoryReport workflow.
type Request struct {
	// Owner and Repo name the repository, as in github.com/OWNER/REPO.
	Owner string `json:"owner"`
	Repo  string `json:"repo"`

	// Since and Until bound the report window, [Since, Until). A zero Until
	// means the time the workflow run started, which is when the report was
	// requested, and a zero Since means DefaultWindow before Until.
	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`

	// MaxPages caps how many pages of each resource the report fetches. Zero
	// means DefaultMaxPages. A resource with more pages is reported as
	// truncated rather than failing the report.
	MaxPages int `json:"max_pages,omitempty"`
}

// resolve validates r and returns its window and page limit, filling in
// defaults relative to now. The workflow passes its run's start time, from
// its history, so that a replay computes the same window.
func (r Request) resolve(now time.Time) (Window, int, error) {
	if r.Owner == "" || r.Repo == "" {
		return Window{}, 0, errors.New("owner and repo are required")
	}
	if r.MaxPages < 0 || r.MaxPages > MaxPagesLimit {
		return Window{}, 0, fmt.Errorf("max pages must be from 1 to %d, or 0 for the default, got %d", MaxPagesLimit, r.MaxPages)
	}
	maxPages := r.MaxPages
	if maxPages == 0 {
		maxPages = DefaultMaxPages
	}

	w := Window{Since: r.Since.UTC(), Until: r.Until.UTC()}
	if w.Until.IsZero() {
		w.Until = now.UTC().Truncate(time.Second)
	}
	if w.Since.IsZero() {
		w.Since = w.Until.Add(-DefaultWindow)
	}
	switch {
	case !w.Since.Before(w.Until):
		return Window{}, 0, fmt.Errorf("since (%s) must be before until (%s)",
			w.Since.Format(time.RFC3339), w.Until.Format(time.RFC3339))
	case w.Until.Sub(w.Since) > MaxWindow:
		return Window{}, 0, fmt.Errorf("the window must not be longer than %d days", MaxWindow/(24*time.Hour))
	}
	return w, maxPages, nil
}

// Window is the half-open time range [Since, Until) that a report covers.
type Window struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// Contains reports whether t falls within the window.
func (w Window) Contains(t time.Time) bool {
	return !t.Before(w.Since) && t.Before(w.Until)
}

// Report is the result of the RepositoryReport workflow.
type Report struct {
	Repository   Repository        `json:"repository"`
	Window       Window            `json:"window"`
	Issues       IssueReport       `json:"issues"`
	PullRequests PullRequestReport `json:"pull_requests"`
	Commits      CommitReport      `json:"commits"`
}

// Repository describes the repository a report covers.
type Repository struct {
	FullName      string `json:"full_name"`
	Description   string `json:"description,omitempty"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Stars         int    `json:"stars"`
	Forks         int    `json:"forks"`
	Archived      bool   `json:"archived,omitempty"`
}

// IssueReport is the issues section of a report.
type IssueReport struct {
	IssueStats

	// Truncated is true if the repository had more pages of issues than the
	// report's page limit, so the counts cover only the pages fetched.
	Truncated bool `json:"truncated,omitempty"`
}

// PullRequestReport is the pull requests section of a report.
type PullRequestReport struct {
	// Opened counts pull requests created in the window.
	Opened int `json:"opened"`
	// Merged counts pull requests merged in the window.
	Merged int `json:"merged"`
	// ClosedUnmerged counts pull requests closed without merging in the
	// window.
	ClosedUnmerged int `json:"closed_unmerged"`
	// StillOpen counts pull requests created in the window that were still
	// open when the report fetched them.
	StillOpen int `json:"still_open"`
	// MedianTimeToMergeSeconds is the median time from creation to merge of
	// the pull requests merged in the window, or zero if none were.
	MedianTimeToMergeSeconds int64 `json:"median_time_to_merge_seconds"`

	// Truncated is true if the report stopped at its page limit.
	Truncated bool `json:"truncated,omitempty"`
}

// CommitReport is the commits section of a report. It covers the default
// branch and dates commits by their committer date, which is what GitHub's
// since and until filters use.
type CommitReport struct {
	// Total counts commits in the window.
	Total int `json:"total"`
	// Weekly counts commits per week, for every week the window touches,
	// oldest first. Weeks start on Monday, UTC.
	Weekly []WeekCount `json:"weekly"`
	// TopContributors lists up to TopContributors GitHub accounts with the
	// most commits, most first.
	TopContributors []AuthorCount `json:"top_contributors"`
	// Contributors counts distinct GitHub accounts with commits.
	Contributors int `json:"contributors"`
	// Unattributed counts commits that GitHub could not link to an account.
	Unattributed int `json:"unattributed"`

	// Truncated is true if the report stopped at its page limit.
	Truncated bool `json:"truncated,omitempty"`
}

// WeekCount is the number of commits in one week.
type WeekCount struct {
	// WeekOf is the Monday that starts the week, as YYYY-MM-DD.
	WeekOf  string `json:"week_of"`
	Commits int    `json:"commits"`
}

// AuthorCount is the number of commits by one GitHub account.
type AuthorCount struct {
	Login   string `json:"login"`
	Commits int    `json:"commits"`
}
