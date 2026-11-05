package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// env returns a getenv function backed by kv.
func env(kv map[string]string) func(string) string {
	return func(key string) string { return kv[key] }
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "version flag", args: []string{"-version"}, wantCode: 0, wantStdout: "durable-mcp dev\n"},
		{name: "help flag", args: []string{"-h"}, wantCode: 0, wantStderr: "durable-mcp worker [flags]"},
		{name: "no arguments prints usage", args: nil, wantCode: 2, wantStderr: "Usage:"},
		{name: "unknown flag", args: []string{"-nope"}, wantCode: 2, wantStderr: "flag provided but not defined"},
		{name: "worker help", args: []string{"worker", "-h"}, wantCode: 0, wantStderr: "-temporal-address host:port"},
		{name: "worker unknown flag", args: []string{"worker", "-nope"}, wantCode: 2, wantStderr: "flag provided but not defined: -nope"},
		{name: "worker extra argument", args: []string{"worker", "now"}, wantCode: 2, wantStderr: `unexpected arguments: ["now"]`},
		{name: "worker empty task queue", args: []string{"worker", "-task-queue="}, wantCode: 2, wantStderr: "-task-queue must not be empty"},
		{
			// The GitHub client is created before connecting to Temporal,
			// so a bad configuration fails without a server.
			name:       "worker refuses to send a token over plain HTTP",
			args:       []string{"worker", "-github-api-url", "http://ghe.example.com/api/v3"},
			wantCode:   1,
			wantStderr: "refusing to send a token over plain HTTP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tt.args, env(map[string]string{"GITHUB_TOKEN": "test-token"}), &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestParseWorkerFlags(t *testing.T) {
	t.Parallel()

	allEnv := map[string]string{
		"TEMPORAL_ADDRESS":       "temporal.internal:7233",
		"TEMPORAL_NAMESPACE":     "reports",
		"DURABLE_MCP_TASK_QUEUE": "reports-queue",
		"GITHUB_API_URL":         "https://ghe.example.com/api/v3/",
		"GITHUB_TOKEN":           "env-token",
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want workerConfig
	}{
		{
			name: "defaults suit a local development server",
			want: workerConfig{
				temporalAddress:   "localhost:7233",
				temporalNamespace: "default",
				taskQueue:         "durable-mcp",
				githubAPIURL:      "https://api.github.com/",
			},
		},
		{
			name: "environment",
			env:  allEnv,
			want: workerConfig{
				temporalAddress:   "temporal.internal:7233",
				temporalNamespace: "reports",
				taskQueue:         "reports-queue",
				githubAPIURL:      "https://ghe.example.com/api/v3/",
				githubToken:       "env-token",
			},
		},
		{
			name: "flags override the environment",
			args: []string{
				"-temporal-address", "127.0.0.1:7233", "-temporal-namespace", "dev",
				"-task-queue", "q", "-github-api-url", "https://api.github.com/",
			},
			env: allEnv,
			want: workerConfig{
				temporalAddress:   "127.0.0.1:7233",
				temporalNamespace: "dev",
				taskQueue:         "q",
				githubAPIURL:      "https://api.github.com/",
				githubToken:       "env-token",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			got, err := parseWorkerFlags(tt.args, env(tt.env), &stderr)
			if err != nil {
				t.Fatalf("parseWorkerFlags() error = %v (stderr: %s)", err, stderr.String())
			}
			if got != tt.want {
				t.Errorf("parseWorkerFlags() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestWorkerReportsConnectionFailure(t *testing.T) {
	t.Parallel()

	// An address that refuses connections: a listener that has closed.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"worker", "-temporal-address", addr}, env(nil), &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	for _, want := range []string{"connect to Temporal at " + addr, "GITHUB_TOKEN is not set"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}
