package report

import (
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	now := window.Until
	tests := []struct {
		name string
		req  Request
		// want are substrings of the error; none means the request is valid.
		want []string
	}{
		{name: "valid", req: Request{Owner: "acme", Repo: "widgets"}},
		{name: "valid window", req: Request{Owner: "acme", Repo: "widgets.go", Since: window.Since, Until: window.Until}},
		{name: "managed user", req: Request{Owner: "mona_acme", Repo: "my-repo_2"}},
		{name: "39-character owner", req: Request{Owner: strings.Repeat("a", 39), Repo: "r"}},
		{name: "100-character repo", req: Request{Owner: "acme", Repo: strings.Repeat("r", 100)}},
		{name: "until within the clock skew", req: Request{Owner: "acme", Repo: "widgets", Until: now.Add(time.Minute)}},

		{name: "missing both", req: Request{}, want: []string{"owner is required", "repo is required"}},
		{name: "missing repo", req: Request{Owner: "acme"}, want: []string{"repo is required"}},
		{name: "owner/repo in owner", req: Request{Owner: "acme/widgets", Repo: "widgets"}, want: []string{"pass the owner and the repository name separately"}},
		{name: "path in repo", req: Request{Owner: "acme", Repo: "widgets/issues"}, want: []string{`repo "widgets/issues" contains '/'`}},
		{name: "40-character owner", req: Request{Owner: strings.Repeat("a", 40), Repo: "r"}, want: []string{"39-character limit"}},
		{name: "101-character repo", req: Request{Owner: "acme", Repo: strings.Repeat("r", 101)}, want: []string{"100-character limit"}},
		{name: "owner starting with a hyphen", req: Request{Owner: "-acme", Repo: "widgets"}, want: []string{"not starting with a hyphen"}},
		{name: "dot in owner", req: Request{Owner: "acme.corp", Repo: "widgets"}, want: []string{"is not a GitHub account name"}},
		{name: "dot segment", req: Request{Owner: "acme", Repo: ".."}, want: []string{`repo ".." is not a repository name`}},
		{name: "space", req: Request{Owner: "acme corp", Repo: "widgets"}, want: []string{"is not a GitHub account name"}},
		{name: "percent", req: Request{Owner: "acme", Repo: "wid%2fgets"}, want: []string{"is not a GitHub repository name"}},
		{name: ".git suffix", req: Request{Owner: "acme", Repo: "widgets.git"}, want: []string{"ends in .git"}},
		{name: "bad window", req: Request{Owner: "acme", Repo: "widgets", Since: now}, want: []string{"must be before until"}},
		{name: "window too long", req: Request{Owner: "acme", Repo: "widgets", Since: now.AddDate(-2, 0, 0)}, want: []string{"longer than 366 days"}},
		{
			name: "since in the future",
			req:  Request{Owner: "acme", Repo: "widgets", Since: now.Add(time.Hour), Until: now.Add(2 * time.Hour)},
			want: []string{"since (2026-11-01T01:00:00Z) is in the future", "until (2026-11-01T02:00:00Z) is in the future"},
		},
		{
			name: "every problem at once",
			req:  Request{Owner: "-x", Repo: "y.git"},
			want: []string{"not starting with a hyphen", "; ", "ends in .git"},
		},
	}
	for _, tt := range tests {
		err := tt.req.Validate(now)
		if len(tt.want) == 0 {
			if err != nil {
				t.Errorf("%s: Validate() = %v, want nil", tt.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", tt.name)
			continue
		}
		for _, w := range tt.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: Validate() = %q, want it to contain %q", tt.name, err, w)
			}
		}
	}
}
