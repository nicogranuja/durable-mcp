package github

import (
	"errors"
	"net/url"
	"testing"
)

func TestGetBuildRequestError(t *testing.T) {
	t.Parallel()

	c, err := New("", WithBaseURL("https://api.github.test/"))
	if err != nil {
		t.Fatal(err)
	}
	// A URL with a non-numeric port cannot be parsed back into a request.
	u := &url.URL{Scheme: "https", Host: "api.github.test:port", Path: "/repos/acme/widgets"}
	if _, err := c.get(t.Context(), u); !errors.Is(err, ErrBuildRequest) {
		t.Errorf("get() error = %v, want ErrBuildRequest", err)
	}
}
