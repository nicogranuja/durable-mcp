package auth_test

import (
	"strings"
	"testing"

	"github.com/nicogranuja/durable-mcp/internal/auth"
)

// Test tokens are obviously fake: they are not in the minted format.
const (
	aliceToken = "test-token-alice-not-a-secret"
	bobToken   = "test-token-bob-not-a-secret"
)

func mustTokens(t *testing.T, entries ...string) *auth.Tokens {
	t.Helper()
	tokens, err := auth.ParseTokens(strings.NewReader(strings.Join(entries, "\n")))
	if err != nil {
		t.Fatalf("ParseTokens: %v", err)
	}
	return tokens
}

func TestVerify(t *testing.T) {
	t.Parallel()

	tokens := mustTokens(t, "# callers", "", auth.Entry("alice", aliceToken), "  "+auth.Entry("bob@example.com", bobToken)+"  ")
	if tokens.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", tokens.Len())
	}
	tests := []struct {
		token string
		want  string // "" means rejected
	}{
		{aliceToken, "alice"},
		{bobToken, "bob@example.com"},
		{"", ""},
		{"test-token-alice-not-a-secreT", ""},
		{aliceToken + " ", ""},
		{auth.Hash(aliceToken), ""}, // the hash is not the token
	}
	for _, tt := range tests {
		c, ok := tokens.Verify(tt.token)
		if ok != (tt.want != "") || c.Name != tt.want {
			t.Errorf("Verify(%q) = %+v, %v, want %q", tt.token, c, ok, tt.want)
		}
	}
	if _, ok := (&auth.Tokens{}).Verify(aliceToken); ok {
		t.Error("an empty token set accepted a token")
	}
}

func TestParseTokensErrors(t *testing.T) {
	t.Parallel()

	alice := auth.Entry("alice", aliceToken)
	tests := []struct {
		name, input, want string
	}{
		{"no separator", "alice", `line 1: want "name:sha256-hex-of-token"`},
		{"bad name", "al ice:" + auth.Hash(aliceToken), `caller name "al ice" must be 1 to 64`},
		{"empty name", ":" + auth.Hash(aliceToken), `caller name "" must be`},
		{"token instead of a hash", "alice:" + auth.TokenPrefix + "abc", "holds a token, not its hash"},
		{"short hash", "alice:abcd", "must be 64 hex characters"},
		{"not hex", "alice:" + strings.Repeat("z", 64), "is not hex"},
		{"hash of the empty token", "alice:" + auth.Hash(""), "hash of an empty token"},
		{"duplicate name", alice + "\n" + "alice:" + auth.Hash(bobToken), `line 2: caller "alice" is listed twice`},
		{"shared token", alice + "\n" + auth.Entry("bob", aliceToken), `callers "alice" and "bob" have the same token`},
	}
	for _, tt := range tests {
		_, err := auth.ParseTokens(strings.NewReader(tt.input))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: ParseTokens() error = %v, want it to contain %q", tt.name, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), auth.Hash(aliceToken)) {
			t.Errorf("%s: error %q echoes a hash", tt.name, err)
		}
	}
}

func TestParseTokenList(t *testing.T) {
	t.Parallel()

	tokens, err := auth.ParseTokenList(auth.Entry("alice", aliceToken) + ", " + auth.Entry("bob", bobToken) + ",")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tokens.Names(), ","); got != "alice,bob" {
		t.Errorf("Names() = %s, want alice,bob", got)
	}
	if _, err := auth.ParseTokenList("alice:nope"); err == nil || !strings.Contains(err.Error(), "entry 1:") {
		t.Errorf("ParseTokenList() error = %v, want one naming the entry", err)
	}
}

func TestNewToken(t *testing.T) {
	t.Parallel()

	a, b := auth.NewToken(), auth.NewToken()
	if a == b || !strings.HasPrefix(a, auth.TokenPrefix) || len(a) != len(auth.TokenPrefix)+26 {
		t.Errorf("NewToken() = %q, %q, want two different %s tokens of 26 random characters", a, b, auth.TokenPrefix)
	}
	tokens := mustTokens(t, auth.Entry("alice", a))
	if c, ok := tokens.Verify(a); !ok || c.Name != "alice" {
		t.Errorf("a minted token does not verify against its entry")
	}
}
