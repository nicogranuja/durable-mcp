package mcpserver

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// A job ID is the workflow ID of the job's RepositoryReport workflow:
// jobIDPrefix followed by 26 base32 characters, which encode 128 or more
// bits. A random ID comes from crypto/rand; an ID for an idempotency key is
// a hash of the key, so the same key always names the same job.
const (
	jobIDPrefix  = "report-"
	jobIDLen     = len(jobIDPrefix) + 26
	minKeyLength = 8
	maxKeyLength = 128
)

// idEncoding is lowercase base32 without padding. Lowercase reads better
// in a workflow ID and matches the case of rand.Text after strings.ToLower.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newJobID returns a random job ID.
func newJobID() string {
	return jobIDPrefix + strings.ToLower(rand.Text()) // 26 characters, 130 bits
}

// jobIDForKey returns the job ID that an idempotency key maps to: the
// first 128 bits of a SHA-256 hash of the key. Hashing keeps the ID a fixed
// length and charset whatever the key is, and keeps the key's text out of
// the workflow ID, which shows in the Temporal UI and logs.
func jobIDForKey(key string) string {
	sum := sha256.Sum256([]byte("durable-mcp report job\x00" + key))
	return jobIDPrefix + idEncoding.EncodeToString(sum[:16])
}

// validateJobID checks that id has the shape of a job ID before it is sent
// to Temporal.
func validateJobID(id string) error {
	if len(id) != jobIDLen || !strings.HasPrefix(id, jobIDPrefix) ||
		strings.Trim(id[len(jobIDPrefix):], "abcdefghijklmnopqrstuvwxyz234567") != "" {
		return fmt.Errorf("%q is not a job ID: a job ID looks like %q and comes from start_report. "+
			"Use the job_id that start_report returned", truncate(id, 80), jobIDPrefix+strings.Repeat("x", 26))
	}
	return nil
}

// validateKey checks an idempotency key: minKeyLength to maxKeyLength
// ASCII letters, digits, '-', '_', '.', and ':'. That fits UUIDs, ULIDs,
// and most generated IDs, and keeps keys free of whitespace, control
// characters, and look-alike Unicode, which would make two keys that read
// the same name different jobs. A short key is likelier to be reused by
// accident for a different report.
func validateKey(key string) error {
	const rule = "idempotency_key must be %d to %d characters of ASCII letters, digits, '-', '_', '.', or ':', " +
		"such as a UUID you generate for each new report"
	switch {
	case len(key) < minKeyLength || len(key) > maxKeyLength:
		return fmt.Errorf(rule+"; got %d characters", minKeyLength, maxKeyLength, utf8.RuneCountInString(key))
	case strings.ContainsFunc(key, func(r rune) bool { return !isKeyChar(r) }):
		i := strings.IndexFunc(key, func(r rune) bool { return !isKeyChar(r) })
		r, _ := utf8.DecodeRuneInString(key[i:])
		return fmt.Errorf(rule+"; got %q at position %d", minKeyLength, maxKeyLength, r, utf8.RuneCountInString(key[:i])+1)
	}
	return nil
}

func isKeyChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == ':'
}

// fingerprint identifies the request a job was started for, so that a
// retry with the same idempotency key can be checked against it. GitHub
// names are not case sensitive, so neither is the fingerprint.
//
// The fingerprint is stored in the job's memo, so its format must never
// change: a retry after an upgrade has to match the jobs started before
// it. It is built from an explicit list of fields, not from the Request's
// JSON, so that a new Request field does not change it by accident. A
// field that affects the report must be added at the end, and only when
// set, so existing fingerprints stay the same.
func fingerprint(req report.Request) string {
	formatTime := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	canonical := strings.Join([]string{
		"report/v1",
		strings.ToLower(req.Owner),
		strings.ToLower(req.Repo),
		formatTime(req.Since),
		formatTime(req.Until),
		strconv.Itoa(req.MaxPages),
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:16])
}

// truncate shortens s to at most n runes, for echoing input in errors.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
