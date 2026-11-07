package mcpserver

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/nicogranuja/durable-mcp/internal/github"
	"github.com/nicogranuja/durable-mcp/internal/report"
)

// TestFailureTaxonomy pins the whole error mapping, end to end: what GitHub
// (or the network) answers, how the client classifies it, the type and
// retry decision of the activity's application error, how many requests
// Temporal makes before giving up, and the failure type, retryability, and
// next step that job_result gives the agent.
//
// Each row runs the real workflow and activities in the Temporal test
// environment, against a fake GitHub API, and passes the workflow's error to
// job_result. The last rows cover failures that do not come from GitHub.
func TestFailureTaxonomy(t *testing.T) {
	t.Parallel()

	reset := strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10)
	status := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			fmt.Fprint(w, body)
		}
	}
	// onIssues answers the issues listing with h and everything else with
	// a small repository.
	onIssues := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/repos/acme/widgets":
				status(http.StatusOK, `{"full_name": "acme/widgets"}`)(w, r)
			case strings.HasSuffix(r.URL.Path, "/issues"):
				h(w, r)
			default:
				status(http.StatusOK, `[]`)(w, r)
			}
		}
	}
	page := func(link string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", link)
			}
			status(http.StatusOK, `[]`)(w, r)
		}
	}

	tests := []struct {
		name    string
		handler http.HandlerFunc // nil: the server is down
		req     report.Request
		opts    []github.Option

		wantRequests int32 // requests that reached the fake GitHub
		wantType     string
		wantRetry    bool   // whether a new report could succeed later
		wantNext     string // part of the next step in job_result's message
	}{
		{
			name:    "401 bad token",
			handler: status(http.StatusUnauthorized, `{"message": "Bad credentials"}`), wantRequests: 1,
			wantType: report.ErrorTypeUnauthorized, wantNext: "must fix GITHUB_TOKEN",
		},
		{
			name:    "403 missing permission",
			handler: status(http.StatusForbidden, `{"message": "Resource not accessible by integration"}`), wantRequests: 1,
			wantType: report.ErrorTypeForbidden, wantNext: "not allowed to read this repository",
		},
		{
			name:    "404 missing repository",
			handler: status(http.StatusNotFound, `{"message": "Not Found"}`), wantRequests: 1,
			wantType: report.ErrorTypeNotFound, wantNext: "Check owner and repo",
		},
		{
			name:    "422 rejected parameters",
			handler: status(http.StatusUnprocessableEntity, `{"message": "Validation Failed"}`), wantRequests: 1,
			wantType: report.ErrorTypeValidationFailed, wantNext: "Fix the arguments",
		},
		{
			name:    "451 blocked repository",
			handler: status(http.StatusUnavailableForLegalReasons, `{"message": "Repository access blocked"}`), wantRequests: 1,
			wantType: report.ErrorTypeRequestRejected, wantNext: "cannot be used for this repository",
		},
		{
			name:    "rate limit that resets after the page's budget",
			handler: status(http.StatusForbidden, `{"message": "API rate limit exceeded"}`),
			// set below: the headers need the handler to write them
			wantRequests: 1,
			wantType:     report.ErrorTypeRateLimited, wantRetry: true, wantNext: "Start a new report later",
		},
		{
			name:    "500 on every attempt",
			handler: status(http.StatusInternalServerError, `{}`), wantRequests: 10,
			wantType: report.ErrorTypeUnavailable, wantRetry: true, wantNext: "Start a new report in a few minutes",
		},
		{
			name:    "408 on every attempt",
			handler: status(http.StatusRequestTimeout, `{}`), wantRequests: 10,
			wantType: report.ErrorTypeUnavailable, wantRetry: true, wantNext: "Start a new report in a few minutes",
		},
		{
			name:    "body that is not JSON",
			handler: status(http.StatusOK, `<html>maintenance</html>`), wantRequests: 10,
			wantType: report.ErrorTypeUnavailable, wantRetry: true, wantNext: "Start a new report in a few minutes",
		},
		{
			name:     "server down",
			handler:  nil,
			wantType: report.ErrorTypeUnavailable, wantRetry: true, wantNext: "Start a new report in a few minutes",
		},
		{
			name:    "oversized page",
			handler: status(http.StatusOK, `{"full_name": "acme/widgets", "description": "`+strings.Repeat("x", 2048)+`"}`),
			opts:    []github.Option{github.WithMaxResponseBytes(1024)}, wantRequests: 1,
			wantType: report.ErrorTypeResponseTooLarge, wantNext: "cannot be used for this repository",
		},
		{
			name:         "next page link off the API",
			handler:      onIssues(page(`<https://elsewhere.test/repos/acme/widgets/issues?page=2>; rel="next"`)),
			wantRequests: 2,
			wantType:     report.ErrorTypeLinkRefused, wantNext: "check the worker's GitHub API URL",
		},
		{
			name: "redirect off the API",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://elsewhere.test/repos/acme/widgets", http.StatusFound)
			},
			wantRequests: 1,
			wantType:     report.ErrorTypeLinkRefused, wantNext: "check the worker's GitHub API URL",
		},
		{
			name: "next page of another repository",
			// The link stays on the API, so the page is fetched, but the
			// next activity refuses the cursor.
			handler:      onIssues(page(`</repos/acme/gadgets/issues?page=2>; rel="next"`)),
			wantRequests: 2,
			wantType:     report.ErrorTypeInternal, wantNext: "tell the user, with the job ID",
		},
		{
			name:         "invalid repository name",
			handler:      status(http.StatusOK, `{}`),
			req:          report.Request{Owner: "acme", Repo: "../widgets"},
			wantRequests: 0,
			wantType:     report.ErrorTypeInvalidRequest, wantNext: "Fix the arguments",
		},
		{
			name:         "invalid window",
			handler:      status(http.StatusOK, `{}`),
			req:          report.Request{Owner: "acme", Repo: "widgets", Since: time.Now(), Until: time.Now().Add(-time.Hour)},
			wantRequests: 0,
			wantType:     report.ErrorTypeInvalidRequest, wantNext: "Fix the arguments",
		},
	}
	// The rate-limit row needs headers.
	for i := range tests {
		if strings.HasPrefix(tests[i].name, "rate limit") {
			body := tests[i].handler
			tests[i].handler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-RateLimit-Limit", "5000")
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", reset)
				body(w, r)
			}
		}
	}

	covered := map[string]bool{}
	for _, tt := range tests {
		covered[tt.wantType] = true
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				tt.handler(w, r)
			}))
			t.Cleanup(srv.Close)
			if tt.handler == nil {
				srv.Close()
			}
			gh, err := github.New("test-token", append([]github.Option{github.WithBaseURL(srv.URL)}, tt.opts...)...)
			if err != nil {
				t.Fatal(err)
			}

			var s testsuite.WorkflowTestSuite
			s.SetLogger(log.NewStructuredLogger(discardLogger()))
			env := s.NewTestWorkflowEnvironment()
			report.Register(env, report.NewActivities(gh))
			req := tt.req
			if req.Owner == "" {
				req = report.Request{Owner: "acme", Repo: "widgets"}
			}
			env.ExecuteWorkflow(report.RepositoryReport, req)
			wfErr := env.GetWorkflowError()
			if wfErr == nil {
				t.Fatal("the report succeeded, want it to fail")
			}
			if n := requests.Load(); tt.handler != nil && n != tt.wantRequests {
				t.Errorf("GitHub got %d requests, want %d", n, tt.wantRequests)
			}

			// The activity's own error, as the pending activity shows it
			// while Temporal retries.
			if appErr, ok := errors.AsType[*temporal.ApplicationError](wfErr); ok {
				if appErr.Type() != tt.wantType {
					t.Errorf("application error type = %q, want %q (error: %v)", appErr.Type(), tt.wantType, wfErr)
				}
				transient := tt.wantType == report.ErrorTypeRateLimited || tt.wantType == report.ErrorTypeUnavailable
				if appErr.NonRetryable() == transient {
					t.Errorf("activity error non-retryable = %v, want %v", appErr.NonRetryable(), !transient)
				}
			} else {
				t.Errorf("workflow error %v has no application error", wfErr)
			}

			assertJobResult(t, wfErr, tt.wantType, tt.wantRetry, tt.wantNext)
		})
	}

	// Failures that do not come from GitHub's answers.
	other := []struct {
		name      string
		err       error
		wantType  string
		wantRetry bool
		wantNext  string
	}{
		{
			name:     "activity timed out on every attempt",
			err:      activityFailure(enumspb.RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil)),
			wantType: failureTimeout, wantRetry: true, wantNext: "Start a new report in a few minutes",
		},
		{
			name:     "type from a run before the prefix",
			err:      activityFailure(enumspb.RETRY_STATE_NON_RETRYABLE_FAILURE, temporal.NewNonRetryableApplicationError("404", "NotFound", nil)),
			wantType: report.ErrorTypeNotFound, wantNext: "Check owner and repo",
		},
		{
			name:     "error type this server does not know",
			err:      temporal.NewApplicationError("boom", "*errors.errorString"),
			wantType: failureUnknown, wantNext: "stop and tell the user",
		},
		{
			name:     "activity panicked",
			err:      activityFailure(enumspb.RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED, errors.New("panic: oops")),
			wantType: failureUnknown, wantNext: "stop and tell the user",
		},
	}
	for _, tt := range other {
		covered[tt.wantType] = true
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertJobResult(t, tt.err, tt.wantType, tt.wantRetry, tt.wantNext)
		})
	}

	// Every failure type is pinned by a row, and every report error type
	// has a failure kind.
	for _, typ := range report.ErrorTypes() {
		if _, ok := failureKinds[typ]; !ok {
			t.Errorf("report error type %q has no entry in failureKinds", typ)
		}
	}
	for typ := range failureKinds {
		if !covered[typ] {
			t.Errorf("failure type %q is not covered by a row of this test", typ)
		}
	}
	if !slices.Contains(report.ErrorTypes(), report.ErrorTypeInternal) {
		t.Error("report.ErrorTypes() does not list report.Internal")
	}
}

// assertJobResult checks what job_result says about a job that failed with
// err.
func assertJobResult(t *testing.T, err error, wantType string, wantRetry bool, wantNext string) {
	t.Helper()
	f := newFakeClient()
	j := f.add(testJobID, enumspb.WORKFLOW_EXECUTION_STATUS_FAILED)
	j.err = err
	_, out, terr := newTools(f).jobResult(t.Context(), nil, JobInput{JobID: testJobID})
	if terr != nil {
		t.Fatalf("jobResult() error = %v, want a failure in the result", terr)
	}
	if out.Status != statusFailed || out.Failure == nil {
		t.Fatalf("jobResult() = %s, want a failed job", mustJSON(out))
	}
	if out.Failure.Type != wantType || out.Failure.Retryable != wantRetry {
		t.Errorf("failure = %+v, want type %q, retryable %v", out.Failure, wantType, wantRetry)
	}
	if !strings.Contains(out.Message, wantNext) {
		t.Errorf("message = %q, want it to contain %q", out.Message, wantNext)
	}
}
