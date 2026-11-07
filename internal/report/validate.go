package report

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nicogranuja/durable-mcp/internal/github"
)

const (
	// maxOwnerLength is GitHub's limit on user and organization names.
	maxOwnerLength = 39
	// maxRepoLength is GitHub's limit on repository names.
	maxRepoLength = 100
	// clockSkew is how far in the future a window may end, for clocks
	// that disagree a little.
	clockSkew = 5 * time.Minute
)

// Validate reports whether a report for r can run, as of now, with an error
// that says what to fix. A client that starts reports calls it first, so
// that a bad request fails at once, with a message about the input, instead
// of as a failed job. It lists every problem, not only the first.
//
// It checks more than the workflow does, which only repeats Resolve:
//
//   - Owner follows GitHub's rules for account names: 1 to 39 letters,
//     digits, and hyphens, not starting with a hyphen. Underscores are
//     allowed for managed-user accounts, such as mona_acme.
//   - Repo has 1 to 100 letters, digits, '-', '_', and '.', is not "." or
//     "..", and has no ".git" suffix, which GitHub strips from clone URLs.
//   - The window is valid (Resolve) and does not start in the future or end
//     more than a few minutes after now.
//
// A name that passes these checks also passes the GitHub client's own
// check, which every request repeats.
func (r Request) Validate(now time.Time) error {
	var problems []string
	ownerErr, repoErr := validateOwner(r.Owner), validateRepo(r.Repo)
	for _, err := range []error{ownerErr, repoErr} {
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	if ownerErr == nil && repoErr == nil && github.ValidateRepo(r.Owner, r.Repo) != nil {
		// Unreachable while the rules above are stricter than the client's.
		problems = append(problems, fmt.Sprintf("%q/%q is not a valid GitHub repository", r.Owner, r.Repo))
	}

	if r.Owner != "" && r.Repo != "" {
		if _, _, err := r.Resolve(now); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if !r.Since.IsZero() && r.Since.After(now) {
		problems = append(problems, fmt.Sprintf("since (%s) is in the future, so the window would be empty; "+
			"use a time before now", r.Since.UTC().Format(time.RFC3339)))
	}
	if !r.Until.IsZero() && r.Until.After(now.Add(clockSkew)) {
		problems = append(problems, fmt.Sprintf("until (%s) is in the future; leave it empty to end the window now",
			r.Until.UTC().Format(time.RFC3339)))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validateOwner checks a repository owner's name against GitHub's rules
// for account names.
func validateOwner(owner string) error {
	switch {
	case owner == "":
		return errors.New("owner is required: the user or organization in github.com/OWNER/REPO")
	case strings.Contains(owner, "/"):
		return fmt.Errorf("owner %q contains '/': pass the owner and the repository name separately, "+
			"as in github.com/OWNER/REPO", truncate(owner, 60))
	case len(owner) > maxOwnerLength:
		return fmt.Errorf("owner %q is longer than GitHub's %d-character limit", truncate(owner, 60), maxOwnerLength)
	case owner[0] == '-' || strings.ContainsFunc(owner, func(r rune) bool { return !isNameChar(r) || r == '.' }):
		return fmt.Errorf("owner %q is not a GitHub account name: use letters, digits, and hyphens, "+
			"not starting with a hyphen", truncate(owner, 60))
	}
	return nil
}

// validateRepo checks a repository name against GitHub's rules.
func validateRepo(repo string) error {
	switch {
	case repo == "":
		return errors.New("repo is required: the repository name, REPO in github.com/OWNER/REPO")
	case strings.Contains(repo, "/"):
		return fmt.Errorf("repo %q contains '/': pass only the repository name, REPO in github.com/OWNER/REPO, "+
			"and the owner separately", truncate(repo, 60))
	case len(repo) > maxRepoLength:
		return fmt.Errorf("repo %q is longer than GitHub's %d-character limit", truncate(repo, 60), maxRepoLength)
	case repo == "." || repo == "..":
		return fmt.Errorf("repo %q is not a repository name", repo)
	case strings.HasSuffix(strings.ToLower(repo), ".git"):
		return fmt.Errorf("repo %q ends in .git: pass the name without it, as in github.com/OWNER/REPO", truncate(repo, 60))
	case strings.ContainsFunc(repo, func(r rune) bool { return !isNameChar(r) }):
		return fmt.Errorf("repo %q is not a GitHub repository name: use letters, digits, '-', '_', and '.'", truncate(repo, 60))
	}
	return nil
}

func isNameChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
}

// truncate shortens s to at most n bytes, for echoing input in errors.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "..."
}
