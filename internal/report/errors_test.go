package report_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/nicogranuja/durable-mcp/internal/github"
	"github.com/nicogranuja/durable-mcp/internal/report"
)

const (
	primaryLimitBody   = `{"message":"API rate limit exceeded for user ID 1."}`
	secondaryLimitBody = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
)

// The activities turn the client's errors into application errors whose
// type and retry options decide what Temporal does next.
func TestActivityErrors(t *testing.T) {
	t.Parallel()

	reset := strconv.FormatInt(time.Now().Add(42*time.Minute).Unix(), 10)

	tests := []struct {
		name          string
		handler       http.HandlerFunc
		req           report.PageRequest
		wantType      string
		wantRetryable bool
		// For rate limits, the expected NextRetryDelay, to within a few
		// seconds of test time and header resolution.
		wantDelay time.Duration
	}{
		{name: "401", handler: respond(http.StatusUnauthorized, `{"message":"Bad credentials"}`), wantType: report.ErrorTypeUnauthorized},
		{name: "403 without a rate limit", handler: respond(http.StatusForbidden, `{"message":"Resource not accessible by integration"}`), wantType: report.ErrorTypeForbidden},
		{name: "404", handler: respond(http.StatusNotFound, `{"message":"Not Found"}`), wantType: report.ErrorTypeNotFound},
		{name: "422", handler: respond(http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`), wantType: report.ErrorTypeValidationFailed},
		{name: "451", handler: respond(http.StatusUnavailableForLegalReasons, `{"message":"Repository access blocked"}`), wantType: report.ErrorTypeRequestRejected},
		{name: "408 is retried", handler: respond(http.StatusRequestTimeout, `{}`), wantType: report.ErrorTypeUnavailable, wantRetryable: true},
		{name: "500", handler: respond(http.StatusInternalServerError, `{}`), wantType: report.ErrorTypeUnavailable, wantRetryable: true},
		{name: "502 with an HTML body", handler: respond(http.StatusBadGateway, `<html>Bad Gateway</html>`), wantType: report.ErrorTypeUnavailable, wantRetryable: true},
		{name: "malformed JSON", handler: respond(http.StatusOK, `[{"number":`), wantType: report.ErrorTypeUnavailable, wantRetryable: true},
		{
			name:     "invalid repository name",
			handler:  respond(http.StatusOK, `[]`),
			req:      report.PageRequest{Owner: "acme", Repo: "../widgets", Window: window},
			wantType: report.ErrorTypeInvalidRequest,
		},
		{
			name:     "cursor for another repository",
			handler:  respond(http.StatusOK, `[]`),
			req:      report.PageRequest{Owner: "acme", Repo: "widgets", Window: window, Cursor: "/repos/acme/gadgets/issues?page=2"},
			wantType: report.ErrorTypeInternal,
		},
		{
			name:     "next page link off the API",
			handler:  respond(http.StatusOK, `[]`, "Link", `<https://elsewhere.test/repos/acme/widgets/issues?page=2>; rel="next"`),
			wantType: report.ErrorTypeLinkRefused,
		},
		{
			name: "redirect off the API",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://elsewhere.test/repos/acme/widgets/issues", http.StatusFound)
			},
			wantType: report.ErrorTypeLinkRefused,
		},
		{
			name: "redirect loop",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, r.URL.String(), http.StatusFound)
			},
			wantType: report.ErrorTypeLinkRefused,
		},
		{
			name: "primary rate limit waits for the reset",
			handler: respond(http.StatusForbidden, primaryLimitBody,
				"X-RateLimit-Limit", "5000", "X-RateLimit-Remaining", "0", "X-RateLimit-Reset", reset),
			wantType: report.ErrorTypeRateLimited, wantRetryable: true, wantDelay: 42 * time.Minute,
		},
		{
			name:     "secondary rate limit waits as long as Retry-After",
			handler:  respond(http.StatusForbidden, secondaryLimitBody, "Retry-After", "300"),
			wantType: report.ErrorTypeRateLimited, wantRetryable: true, wantDelay: 5 * time.Minute,
		},
		{
			name:     "secondary rate limit waits at least a minute",
			handler:  respond(http.StatusTooManyRequests, secondaryLimitBody, "Retry-After", "5"),
			wantType: report.ErrorTypeRateLimited, wantRetryable: true, wantDelay: time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			acts := newActivities(t, tt.handler)
			env := newActivityEnv(t, acts)
			req := tt.req
			if req.Owner == "" {
				req = pageRequest("", 1)
			}

			_, err := env.ExecuteActivity(acts.FetchIssuesPage, req)
			appErr, ok := errors.AsType[*temporal.ApplicationError](err)
			if !ok {
				t.Fatalf("ExecuteActivity() error = %v, want an application error", err)
			}
			if appErr.Type() != tt.wantType {
				t.Errorf("error type = %q, want %q (error: %v)", appErr.Type(), tt.wantType, err)
			}
			if appErr.NonRetryable() == tt.wantRetryable {
				t.Errorf("non-retryable = %v, want %v", appErr.NonRetryable(), !tt.wantRetryable)
			}

			delay := appErr.NextRetryDelay()
			if tt.wantDelay == 0 {
				if delay != 0 {
					t.Errorf("NextRetryDelay = %s, want none, so the retry policy applies", delay)
				}
				return
			}
			if delay < tt.wantDelay-5*time.Second || delay > tt.wantDelay+time.Second {
				t.Errorf("NextRetryDelay = %s, want about %s", delay, tt.wantDelay)
			}
			if appErr.Category() != temporal.ApplicationErrorCategoryBenign {
				t.Errorf("category = %v, want benign: rate limits are expected", appErr.Category())
			}
		})
	}
}

func TestActivityNetworkErrorIsRetryable(t *testing.T) {
	t.Parallel()

	// A server that is gone: connections are refused.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	gh, err := github.New("test-token", github.WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	acts := report.NewActivities(gh)
	env := newActivityEnv(t, acts)

	_, err = env.ExecuteActivity(acts.FetchRepository, report.RepositoryRequest{Owner: "acme", Repo: "widgets"})
	appErr, ok := errors.AsType[*temporal.ApplicationError](err)
	if !ok || appErr.Type() != report.ErrorTypeUnavailable || appErr.NonRetryable() {
		t.Fatalf("ExecuteActivity() error = %v, want a retryable %s error", err, report.ErrorTypeUnavailable)
	}
}
