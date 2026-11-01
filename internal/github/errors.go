package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Sentinel errors for classifying failures with errors.Is.
var (
	// ErrUnauthorized matches 401 responses: the token is missing, invalid,
	// or expired. Retrying will not help.
	ErrUnauthorized = errors.New("github: unauthorized")

	// ErrForbidden matches 403 responses that are not rate limits: the
	// token lacks a required permission or scope.
	ErrForbidden = errors.New("github: forbidden")

	// ErrNotFound matches 404 responses. GitHub also answers 404 for
	// private resources the token is not allowed to see.
	ErrNotFound = errors.New("github: not found")

	// ErrValidation matches 422 responses: GitHub rejected the request
	// parameters.
	ErrValidation = errors.New("github: validation failed")

	// ErrServer matches 5xx responses, which are usually transient.
	ErrServer = errors.New("github: server error")

	// ErrResponseTooLarge is returned when a successful response body
	// exceeds the limit set with WithMaxResponseBytes.
	ErrResponseTooLarge = errors.New("github: response too large")

	// ErrInvalidArgument is returned for invalid method arguments, before
	// any request is sent.
	ErrInvalidArgument = errors.New("github: invalid argument")
)

// APIError describes a non-2xx response from the GitHub API.
type APIError struct {
	// StatusCode is the HTTP status code.
	StatusCode int
	// Method and Path identify the request. Path excludes the query string.
	Method string
	Path   string
	// Message is GitHub's error message, if the body contained one.
	Message string
	// DocumentationURL links to GitHub's docs for the error, if provided.
	DocumentationURL string
	// Errors holds field-level details, typically for 422 responses.
	Errors []FieldError
	// RequestID is the X-GitHub-Request-Id header. Quote it when asking
	// GitHub support about a failed request.
	RequestID string
}

// Error implements the error interface.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "github: %s %s: %d", e.Method, e.Path, e.StatusCode)
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	if msg != "" {
		b.WriteString(" " + msg)
	}
	if len(e.Errors) > 0 {
		details := make([]string, len(e.Errors))
		for i, fe := range e.Errors {
			details[i] = fe.String()
		}
		b.WriteString(" (" + strings.Join(details, "; ") + ")")
	}
	return b.String()
}

// Is reports whether e matches one of the status sentinels, so callers can
// write errors.Is(err, ErrNotFound) without inspecting status codes.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrValidation:
		return e.StatusCode == http.StatusUnprocessableEntity
	case ErrServer:
		return e.StatusCode >= 500 && e.StatusCode <= 599
	}
	return false
}

// FieldError is one entry of the "errors" array in an error response.
type FieldError struct {
	Resource string `json:"resource"`
	Field    string `json:"field"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// UnmarshalJSON accepts both the documented object form and the plain
// strings that some endpoints return instead.
func (fe *FieldError) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &fe.Message)
	}
	type plain FieldError // drops this method to avoid recursion
	return json.Unmarshal(data, (*plain)(fe))
}

// String returns a short description such as "Issue.title: missing_field".
func (fe FieldError) String() string {
	subject := fe.Field
	if fe.Resource != "" && fe.Field != "" {
		subject = fe.Resource + "." + fe.Field
	}
	detail := fe.Message
	if detail == "" {
		detail = fe.Code
	}
	switch {
	case subject == "":
		return detail
	case detail == "":
		return subject
	default:
		return subject + ": " + detail
	}
}

// newAPIError builds an APIError from a non-2xx response. The body is
// decoded on a best-effort basis: proxies and load balancers in front of
// GitHub can return HTML or empty bodies, and the status code alone is
// still enough to classify the error.
func newAPIError(resp *http.Response, body []byte) *APIError {
	e := &APIError{
		StatusCode: resp.StatusCode,
		Method:     resp.Request.Method,
		Path:       resp.Request.URL.Path,
		RequestID:  resp.Header.Get("X-GitHub-Request-Id"),
	}
	var payload struct {
		Message          string       `json:"message"`
		DocumentationURL string       `json:"documentation_url"`
		Errors           []FieldError `json:"errors"`
	}
	if json.Unmarshal(body, &payload) == nil {
		e.Message = payload.Message
		e.DocumentationURL = payload.DocumentationURL
		e.Errors = payload.Errors
	}
	return e
}
