package github

import (
	"context"
	"fmt"
	"time"
)

// User is a GitHub account as embedded in other resources.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	// Type is "User", "Bot", or "Organization".
	Type string `json:"type"`
}

// Repository is the subset of GitHub's repository resource that reports use.
type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         User   `json:"owner"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Language      string `json:"language"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`

	StargazersCount int `json:"stargazers_count"`
	ForksCount      int `json:"forks_count"`
	// OpenIssuesCount includes open pull requests, as GitHub counts them.
	OpenIssuesCount int `json:"open_issues_count"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// PushedAt is zero for a repository that has never been pushed to.
	PushedAt time.Time `json:"pushed_at"`
}

// GetRepository fetches a repository by owner and name.
func (c *Client) GetRepository(ctx context.Context, owner, repo string) (*Repository, error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	var r Repository
	if err := c.getJSON(ctx, c.endpoint("repos", owner, repo), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// validateRepo checks owner and repo before they become URL path segments.
// GitHub names only use ASCII letters, digits, '-', '_', and '.', so
// anything else is rejected rather than escaped. In particular, "/", "..",
// and "%" could otherwise change which endpoint the request reaches.
func validateRepo(owner, repo string) error {
	if !validName(owner) {
		return fmt.Errorf("%w: invalid owner %q", ErrInvalidArgument, owner)
	}
	if !validName(repo) {
		return fmt.Errorf("%w: invalid repository name %q", ErrInvalidArgument, repo)
	}
	return nil
}

func validName(s string) bool {
	if s == "" || len(s) > 100 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}
