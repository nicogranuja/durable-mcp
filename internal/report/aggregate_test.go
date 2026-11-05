package report

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

// window is the test report window: October 2026, which starts on a
// Thursday and ends on a Saturday, so it touches five ISO weeks.
var window = Window{
	Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	Until: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
}

func day(d int) time.Time         { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
func dayPtr(d int) *time.Time     { return new(day(d)) }
func before() time.Time           { return window.Since.Add(-time.Hour) }
func after() time.Time            { return window.Until.Add(time.Hour) }
func login(s string) *github.User { return &github.User{Login: s} }

func TestResolve(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 4, 15, 30, 45, 123e6, time.FixedZone("PST", -8*3600))
	nowUTC := time.Date(2026, 11, 4, 23, 30, 45, 0, time.UTC)

	tests := []struct {
		name         string
		req          Request
		wantWindow   Window
		wantMaxPages int
		wantErr      string
	}{
		{
			name:         "defaults end when the workflow started",
			req:          Request{Owner: "acme", Repo: "widgets"},
			wantWindow:   Window{Since: nowUTC.Add(-DefaultWindow), Until: nowUTC},
			wantMaxPages: DefaultMaxPages,
		},
		{
			name:         "explicit window in another zone is converted to UTC",
			req:          Request{Owner: "acme", Repo: "widgets", Since: window.Since.In(now.Location()), Until: window.Until, MaxPages: 5},
			wantWindow:   window,
			wantMaxPages: 5,
		},
		{
			name:         "since alone ends now",
			req:          Request{Owner: "acme", Repo: "widgets", Since: window.Since},
			wantWindow:   Window{Since: window.Since, Until: nowUTC},
			wantMaxPages: DefaultMaxPages,
		},
		{
			name:         "until alone looks back the default window",
			req:          Request{Owner: "acme", Repo: "widgets", Until: window.Until},
			wantWindow:   Window{Since: window.Until.Add(-DefaultWindow), Until: window.Until},
			wantMaxPages: DefaultMaxPages,
		},
		{name: "missing owner", req: Request{Repo: "widgets"}, wantErr: "owner and repo are required"},
		{name: "missing repo", req: Request{Owner: "acme"}, wantErr: "owner and repo are required"},
		{
			name:    "since after until",
			req:     Request{Owner: "acme", Repo: "widgets", Since: window.Until, Until: window.Since},
			wantErr: "must be before until",
		},
		{
			name:    "empty window",
			req:     Request{Owner: "acme", Repo: "widgets", Since: window.Since, Until: window.Since},
			wantErr: "must be before until",
		},
		{
			name:    "window too long",
			req:     Request{Owner: "acme", Repo: "widgets", Since: window.Until.Add(-MaxWindow - time.Second), Until: window.Until},
			wantErr: "must not be longer than 366 days",
		},
		{name: "negative max pages", req: Request{Owner: "acme", Repo: "widgets", MaxPages: -1}, wantErr: "max pages must be from 1 to 200"},
		{name: "too many pages", req: Request{Owner: "acme", Repo: "widgets", MaxPages: MaxPagesLimit + 1}, wantErr: "max pages must be from 1 to 200"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w, maxPages, err := tt.req.resolve(now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolve() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve() error = %v", err)
			}
			if !w.Since.Equal(tt.wantWindow.Since) || !w.Until.Equal(tt.wantWindow.Until) {
				t.Errorf("window = %v, want %v", w, tt.wantWindow)
			}
			if w.Since.Location() != time.UTC || w.Until.Location() != time.UTC {
				t.Errorf("window %v is not in UTC", w)
			}
			if maxPages != tt.wantMaxPages {
				t.Errorf("max pages = %d, want %d", maxPages, tt.wantMaxPages)
			}
		})
	}
}

func TestWindowContains(t *testing.T) {
	t.Parallel()

	tests := []struct {
		t    time.Time
		want bool
	}{
		{window.Since.Add(-time.Nanosecond), false},
		{window.Since, true},
		{day(15), true},
		{window.Until.Add(-time.Nanosecond), true},
		{window.Until, false},
	}
	for _, tt := range tests {
		if got := window.Contains(tt.t); got != tt.want {
			t.Errorf("Contains(%s) = %v, want %v", tt.t.Format(time.RFC3339Nano), got, tt.want)
		}
	}
}

func TestPageIssueStats(t *testing.T) {
	t.Parallel()

	issues := []github.Issue{
		{Number: 1, State: "open", CreatedAt: day(2)},
		{Number: 2, State: "closed", StateReason: "completed", CreatedAt: day(3), ClosedAt: dayPtr(4)},
		{Number: 3, State: "closed", StateReason: "not_planned", CreatedAt: before(), ClosedAt: dayPtr(5)},
		{Number: 4, State: "closed", StateReason: "duplicate", CreatedAt: before(), ClosedAt: dayPtr(6)},
		{Number: 5, State: "closed", CreatedAt: before(), ClosedAt: dayPtr(7)},
		// Updated in the window, but neither opened nor closed in it.
		{Number: 6, State: "open", StateReason: "reopened", CreatedAt: before()},
		// Opened in the window and closed after it.
		{Number: 7, State: "closed", StateReason: "completed", CreatedAt: day(8), ClosedAt: new(after())},
		// Opened after the window.
		{Number: 8, State: "open", CreatedAt: after()},
	}

	got := pageIssueStats(issues, window)
	want := IssueStats{
		Opened:    3,
		Closed:    4,
		ClosedAs:  CloseReasons{Completed: 1, NotPlanned: 1, Duplicate: 1, Other: 1},
		StillOpen: 1,
	}
	if got != want {
		t.Errorf("pageIssueStats() = %+v, want %+v", got, want)
	}
}

func TestPagePullRequestStats(t *testing.T) {
	t.Parallel()

	prs := []github.PullRequest{
		{Number: 1, State: "open", CreatedAt: day(20), UpdatedAt: after()},
		{Number: 2, State: "closed", CreatedAt: day(10), UpdatedAt: day(12), MergedAt: new(day(10).Add(90 * time.Minute)), ClosedAt: new(day(10).Add(90 * time.Minute))},
		{Number: 3, State: "closed", CreatedAt: before(), UpdatedAt: day(9), MergedAt: dayPtr(9), ClosedAt: dayPtr(9)},
		{Number: 4, State: "closed", CreatedAt: day(5), UpdatedAt: day(6), ClosedAt: dayPtr(6)},
		// Merged after the window: opened in it, but not merged in it.
		{Number: 5, State: "closed", CreatedAt: day(4), UpdatedAt: after(), MergedAt: new(after()), ClosedAt: new(after())},
	}

	got, past := pagePullRequestStats(prs, window)
	want := PullRequestStats{
		Opened:         4,
		Merged:         2,
		ClosedUnmerged: 1,
		StillOpen:      1,
		MergeSeconds:   []int64{90 * 60, int64(day(9).Sub(before()) / time.Second)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pagePullRequestStats() = %+v, want %+v", got, want)
	}
	if past {
		t.Error("pastWindow = true for a page with no pull request older than the window")
	}

	// Sorted newest first, the first pull request updated before the
	// window ends the listing; nothing after it counts.
	older := append(slices.Clone(prs[:1]),
		github.PullRequest{Number: 6, State: "closed", CreatedAt: before(), UpdatedAt: before(), ClosedAt: new(before())},
		github.PullRequest{Number: 7, State: "open", CreatedAt: day(3), UpdatedAt: day(3)}, // out of order; ignored
	)
	got, past = pagePullRequestStats(older, window)
	if want := (PullRequestStats{Opened: 1, StillOpen: 1}); !reflect.DeepEqual(got, want) || !past {
		t.Errorf("pagePullRequestStats() = %+v, %v; want %+v, true", got, past, want)
	}
}

func commit(d time.Time, author *github.User) github.Commit {
	return github.Commit{
		Commit: github.GitCommit{
			Author:    github.GitSignature{Date: d.Add(-24 * time.Hour)},
			Committer: github.GitSignature{Date: d},
		},
		Author: author,
	}
}

func TestPageCommitStats(t *testing.T) {
	t.Parallel()

	commits := []github.Commit{
		commit(day(31), login("mona")), // week 4 (Mon Oct 26)
		commit(day(26), login("hubot")),
		commit(day(14), login("mona")), // week 2 (Mon Oct 12)
		commit(day(4), nil),            // week 0 (Mon Sep 28), a Sunday
		commit(day(1), &github.User{}), // week 0, no login
		commit(after(), login("mona")), // outside the window
		commit(before(), login("mona")),
	}

	got := pageCommitStats(commits, window)
	want := CommitStats{
		Total:        5,
		Weekly:       []int{2, 0, 1, 0, 2},
		Authors:      []AuthorCount{{"hubot", 1}, {"mona", 2}},
		Unattributed: 2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pageCommitStats() = %+v, want %+v", got, want)
	}

	if got := pageCommitStats(commits[5:], window); !reflect.DeepEqual(got, CommitStats{}) {
		t.Errorf("pageCommitStats() with no commits in the window = %+v, want the zero value", got)
	}
}

func TestCommitReport(t *testing.T) {
	t.Parallel()

	var s CommitStats
	s.add(CommitStats{Total: 3, Weekly: []int{1, 2, 0, 0, 0}, Authors: []AuthorCount{{"bob", 1}, {"mona", 2}}})
	s.add(CommitStats{}) // a page with nothing in the window
	s.add(CommitStats{Total: 4, Weekly: []int{0, 0, 0, 1, 1}, Authors: []AuthorCount{{"alice", 2}, {"mona", 1}}, Unattributed: 2})

	got := s.report(window, true)
	want := CommitReport{
		Total: 7,
		Weekly: []WeekCount{
			{"2026-09-28", 1}, {"2026-10-05", 2}, {"2026-10-12", 0}, {"2026-10-19", 1}, {"2026-10-26", 1},
		},
		// mona's commits from both pages add up to 3.
		TopContributors: []AuthorCount{{"mona", 3}, {"alice", 2}, {"bob", 1}},
		Contributors:    3,
		Unattributed:    2,
		Truncated:       true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("report() =\n%+v\nwant\n%+v", got, want)
	}

	// An empty report still lists every week, and an empty top list.
	empty := CommitStats{}.report(window, false)
	if len(empty.Weekly) != 5 || empty.Weekly[4].Commits != 0 || len(empty.TopContributors) != 0 {
		t.Errorf("empty report = %+v, want five zero weeks and no contributors", empty)
	}
}

func TestTopContributorsAreCappedAndTieBroken(t *testing.T) {
	t.Parallel()

	var authors []AuthorCount
	for _, l := range []string{"m", "l", "k", "j", "i", "h", "g", "f", "e", "d", "c", "b", "a"} {
		authors = append(authors, AuthorCount{Login: l, Commits: 1})
	}
	authors = append(authors, AuthorCount{Login: "z", Commits: 5})

	var s CommitStats
	s.add(CommitStats{Total: 18, Authors: mergeAuthors(nil, authors)})
	top := s.report(window, false).TopContributors

	want := []AuthorCount{{"z", 5}, {"a", 1}, {"b", 1}, {"c", 1}, {"d", 1}, {"e", 1}, {"f", 1}, {"g", 1}, {"h", 1}, {"i", 1}}
	if !reflect.DeepEqual(top, want) {
		t.Errorf("TopContributors = %v, want %v", top, want)
	}
}

// The workflow adds pages up in the order they arrive. The report must not
// depend on that order.
func TestAggregationIsOrderIndependent(t *testing.T) {
	t.Parallel()

	pages := []CommitStats{
		{Total: 2, Weekly: []int{2, 0, 0, 0, 0}, Authors: []AuthorCount{{"a", 1}, {"b", 1}}},
		{Total: 3, Weekly: []int{0, 0, 3, 0, 0}, Authors: []AuthorCount{{"b", 2}, {"c", 1}}},
		{Total: 1, Weekly: []int{0, 0, 0, 0, 1}, Authors: []AuthorCount{{"a", 1}}},
	}
	prPages := []PullRequestStats{
		{Merged: 2, MergeSeconds: []int64{50, 10}},
		{Merged: 1, MergeSeconds: []int64{30}},
		{Merged: 1, MergeSeconds: []int64{20}},
	}

	var want CommitReport
	var wantPR PullRequestReport
	for i, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}} {
		var s CommitStats
		var p PullRequestStats
		for _, j := range order {
			s.add(pages[j])
			p.add(prPages[j])
		}
		got, gotPR := s.report(window, false), p.report(false)
		if i == 0 {
			want, wantPR = got, gotPR
			continue
		}
		if !reflect.DeepEqual(got, want) || gotPR != wantPR {
			t.Errorf("order %v: report = %+v, %+v; want %+v, %+v", order, got, gotPR, want, wantPR)
		}
	}
	if wantPR.MedianTimeToMergeSeconds != 25 {
		t.Errorf("median = %d, want 25", wantPR.MedianTimeToMergeSeconds)
	}
}

func TestIssueStatsAdd(t *testing.T) {
	t.Parallel()

	var s IssueStats
	s.add(IssueStats{Opened: 1, Closed: 2, ClosedAs: CloseReasons{Completed: 1, Other: 1}, StillOpen: 1})
	s.add(IssueStats{Opened: 3, Closed: 2, ClosedAs: CloseReasons{NotPlanned: 1, Duplicate: 1}})
	want := IssueStats{Opened: 4, Closed: 4, ClosedAs: CloseReasons{Completed: 1, NotPlanned: 1, Duplicate: 1, Other: 1}, StillOpen: 1}
	if s != want {
		t.Errorf("sum = %+v, want %+v", s, want)
	}
}

func TestMedian(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   []int64
		want int64
	}{
		{nil, 0},
		{[]int64{7}, 7},
		{[]int64{9, 1, 5}, 5},
		{[]int64{4, 1, 3, 2}, 2}, // (2+3)/2, rounded down
		{[]int64{10, 20}, 15},
	}
	for _, tt := range tests {
		in := slices.Clone(tt.in)
		if got := median(in); got != tt.want {
			t.Errorf("median(%v) = %d, want %d", tt.in, got, tt.want)
		}
		if !slices.Equal(in, tt.in) {
			t.Errorf("median modified its input: %v, was %v", in, tt.in)
		}
	}
}

func TestWeeks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), "2026-10-26"},                       // Monday
		{time.Date(2026, 11, 1, 23, 59, 59, 0, time.UTC), "2026-10-26"},                     // Sunday
		{time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), "2025-12-29"},                        // across a year
		{time.Date(2026, 10, 26, 1, 0, 0, 0, time.FixedZone("CEST", 2*3600)), "2026-10-19"}, // still Sunday in UTC
	}
	for _, tt := range tests {
		if got := weekStart(tt.t).Format(time.DateOnly); got != tt.want {
			t.Errorf("weekStart(%s) = %s, want %s", tt.t, got, tt.want)
		}
	}

	oneWeek := Window{Since: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	if n := numWeeks(oneWeek); n != 1 {
		t.Errorf("numWeeks(Monday to Monday) = %d, want 1", n)
	}
	if n := numWeeks(window); n != 5 {
		t.Errorf("numWeeks(October 2026) = %d, want 5", n)
	}
	year := Window{Since: window.Until.Add(-MaxWindow), Until: window.Until}
	if n := numWeeks(year); n > 54 {
		t.Errorf("numWeeks(MaxWindow) = %d, want at most 54", n)
	}
}
