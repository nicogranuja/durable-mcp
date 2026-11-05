package report

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

// This file holds the partial aggregates that activities compute for one
// page and the workflow adds up. Activities build them with the page*
// functions; the workflow combines them with the add methods and turns the
// totals into a Report with the report methods.
//
// The add and report methods run in workflow code, so they must be
// deterministic: no clocks, no I/O, and no iteration over maps, whose order
// Go randomizes. The partials use slices and fixed fields instead of maps,
// so the workflow never ranges over one.

// IssueStats counts issue activity, for one page or a whole report.
type IssueStats struct {
	// Opened counts issues created in the window.
	Opened int `json:"opened"`
	// Closed counts issues closed in the window, broken down in ClosedAs.
	Closed   int          `json:"closed"`
	ClosedAs CloseReasons `json:"closed_as"`
	// StillOpen counts issues created in the window that were still open
	// when the report fetched them.
	StillOpen int `json:"still_open"`
}

// CloseReasons breaks closed issues down by GitHub's state_reason.
type CloseReasons struct {
	Completed  int `json:"completed"`
	NotPlanned int `json:"not_planned"`
	Duplicate  int `json:"duplicate"`
	// Other counts issues closed with no reason, as older issues are, or
	// with a reason this report does not know.
	Other int `json:"other"`
}

// pageIssueStats counts the issues on one page that fall in w.
func pageIssueStats(issues []github.Issue, w Window) IssueStats {
	var s IssueStats
	for _, is := range issues {
		if w.Contains(is.CreatedAt) {
			s.Opened++
			if is.State == "open" {
				s.StillOpen++
			}
		}
		if is.ClosedAt != nil && w.Contains(*is.ClosedAt) {
			s.Closed++
			switch is.StateReason {
			case "completed":
				s.ClosedAs.Completed++
			case "not_planned":
				s.ClosedAs.NotPlanned++
			case "duplicate":
				s.ClosedAs.Duplicate++
			default:
				s.ClosedAs.Other++
			}
		}
	}
	return s
}

func (s *IssueStats) add(o IssueStats) {
	s.Opened += o.Opened
	s.Closed += o.Closed
	s.ClosedAs.Completed += o.ClosedAs.Completed
	s.ClosedAs.NotPlanned += o.ClosedAs.NotPlanned
	s.ClosedAs.Duplicate += o.ClosedAs.Duplicate
	s.ClosedAs.Other += o.ClosedAs.Other
	s.StillOpen += o.StillOpen
}

// PullRequestStats counts pull request activity, for one page or a whole
// report.
type PullRequestStats struct {
	Opened         int `json:"opened"`
	Merged         int `json:"merged"`
	ClosedUnmerged int `json:"closed_unmerged"`
	StillOpen      int `json:"still_open"`

	// MergeSeconds holds, for each pull request merged in the window, the
	// seconds from creation to merge. A median cannot be combined from
	// per-page medians, so pages carry the durations themselves: one number
	// per merged pull request, rather than the pull request.
	MergeSeconds []int64 `json:"merge_seconds,omitempty"`
}

// pagePullRequestStats counts the pull requests on one page that fall in w.
// It expects pull requests sorted by update time, newest first, and reports
// whether the page reached one last updated before the window: every later
// pull request is older still, so the listing can stop there.
func pagePullRequestStats(prs []github.PullRequest, w Window) (s PullRequestStats, pastWindow bool) {
	for _, pr := range prs {
		if pr.UpdatedAt.Before(w.Since) {
			return s, true
		}
		if w.Contains(pr.CreatedAt) {
			s.Opened++
			if pr.State == "open" {
				s.StillOpen++
			}
		}
		switch {
		case pr.MergedAt != nil:
			if w.Contains(*pr.MergedAt) {
				s.Merged++
				s.MergeSeconds = append(s.MergeSeconds, int64(pr.MergedAt.Sub(pr.CreatedAt)/time.Second))
			}
		case pr.ClosedAt != nil && w.Contains(*pr.ClosedAt):
			s.ClosedUnmerged++
		}
	}
	return s, false
}

func (s *PullRequestStats) add(o PullRequestStats) {
	s.Opened += o.Opened
	s.Merged += o.Merged
	s.ClosedUnmerged += o.ClosedUnmerged
	s.StillOpen += o.StillOpen
	s.MergeSeconds = append(s.MergeSeconds, o.MergeSeconds...)
}

func (s PullRequestStats) report(truncated bool) PullRequestReport {
	return PullRequestReport{
		Opened:                   s.Opened,
		Merged:                   s.Merged,
		ClosedUnmerged:           s.ClosedUnmerged,
		StillOpen:                s.StillOpen,
		MedianTimeToMergeSeconds: median(s.MergeSeconds),
		Truncated:                truncated,
	}
}

// median returns the median of xs, or zero for an empty slice. It does not
// modify xs.
func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(xs))
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return s[mid-1] + (s[mid]-s[mid-1])/2
}

// CommitStats counts commits, for one page or a whole report.
type CommitStats struct {
	// Total counts commits in the window.
	Total int `json:"total"`
	// Weekly[i] counts commits in week i of the window, where week 0 is the
	// week that contains the window's start (see weekIndex). It is nil if
	// there are no commits.
	Weekly []int `json:"weekly,omitempty"`
	// Authors counts commits per GitHub account, sorted by login, with each
	// login at most once.
	Authors []AuthorCount `json:"authors,omitempty"`
	// Unattributed counts commits that GitHub could not link to an account.
	Unattributed int `json:"unattributed"`
}

// pageCommitStats counts the commits on one page that fall in w, dated by
// their committer date.
func pageCommitStats(commits []github.Commit, w Window) CommitStats {
	var s CommitStats
	byLogin := map[string]int{}
	for _, c := range commits {
		date := c.Commit.Committer.Date
		if !w.Contains(date) {
			continue
		}
		s.Total++
		if s.Weekly == nil {
			s.Weekly = make([]int, numWeeks(w))
		}
		s.Weekly[weekIndex(w, date)]++
		if c.Author != nil && c.Author.Login != "" {
			byLogin[c.Author.Login]++
		} else {
			s.Unattributed++
		}
	}
	for login, n := range byLogin {
		s.Authors = append(s.Authors, AuthorCount{Login: login, Commits: n})
	}
	slices.SortFunc(s.Authors, func(a, b AuthorCount) int { return strings.Compare(a.Login, b.Login) })
	return s
}

func (s *CommitStats) add(o CommitStats) {
	s.Total += o.Total
	if len(o.Weekly) > len(s.Weekly) {
		s.Weekly = append(s.Weekly, make([]int, len(o.Weekly)-len(s.Weekly))...)
	}
	for i, n := range o.Weekly {
		s.Weekly[i] += n
	}
	s.Authors = mergeAuthors(s.Authors, o.Authors)
	s.Unattributed += o.Unattributed
}

// mergeAuthors combines two author lists into one sorted by login, adding
// up the commits of logins that appear in both.
func mergeAuthors(a, b []AuthorCount) []AuthorCount {
	all := slices.Concat(a, b)
	slices.SortStableFunc(all, func(x, y AuthorCount) int { return strings.Compare(x.Login, y.Login) })
	merged := all[:0]
	for _, c := range all {
		if n := len(merged); n > 0 && merged[n-1].Login == c.Login {
			merged[n-1].Commits += c.Commits
			continue
		}
		merged = append(merged, c)
	}
	return merged
}

func (s CommitStats) report(w Window, truncated bool) CommitReport {
	first := weekStart(w.Since)
	weekly := make([]WeekCount, numWeeks(w))
	for i := range weekly {
		weekly[i].WeekOf = first.AddDate(0, 0, 7*i).Format(time.DateOnly)
		if i < len(s.Weekly) {
			weekly[i].Commits = s.Weekly[i]
		}
	}

	// Most commits first; ties in login order, so the result does not
	// depend on the order pages arrived in.
	top := slices.Clone(s.Authors)
	slices.SortFunc(top, func(a, b AuthorCount) int {
		return cmp.Or(cmp.Compare(b.Commits, a.Commits), strings.Compare(a.Login, b.Login))
	})
	return CommitReport{
		Total:           s.Total,
		Weekly:          weekly,
		TopContributors: slices.Clip(top[:min(TopContributors, len(top))]),
		Contributors:    len(s.Authors),
		Unattributed:    s.Unattributed,
		Truncated:       truncated,
	}
}

const week = 7 * 24 * time.Hour

// weekStart returns midnight UTC at the start of the Monday of t's week.
func weekStart(t time.Time) time.Time {
	t = t.UTC()
	sinceMonday := (int(t.Weekday()) + 6) % 7
	return time.Date(t.Year(), t.Month(), t.Day()-sinceMonday, 0, 0, 0, 0, time.UTC)
}

// weekIndex returns the week of w that t falls in, counting from 0 for the
// week that contains w.Since.
func weekIndex(w Window, t time.Time) int {
	return int(weekStart(t).Sub(weekStart(w.Since)) / week)
}

// numWeeks returns how many weeks w touches.
func numWeeks(w Window) int {
	return weekIndex(w, w.Until.Add(-time.Nanosecond)) + 1
}
