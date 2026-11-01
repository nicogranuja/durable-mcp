// Package github is a small client for the parts of the GitHub REST API that
// the repository report needs: repositories, issues, pull requests, and
// commits.
//
// It is written directly on net/http rather than on a generated SDK so that
// the behaviour that matters for long-running jobs is explicit and testable:
// authentication headers, per-request timeouts, response size limits,
// Link-header pagination, and rate-limit handling.
//
// # Errors
//
// Every non-2xx response is returned as an [*APIError]. Classify errors with
// [errors.Is] against the sentinels ([ErrUnauthorized], [ErrForbidden],
// [ErrNotFound], [ErrValidation], [ErrServer]) and use [errors.As] to read the
// details. Transport failures, including context cancellation and timeouts,
// are wrapped so that errors.Is(err, context.Canceled) and
// errors.Is(err, context.DeadlineExceeded) work as expected.
package github
