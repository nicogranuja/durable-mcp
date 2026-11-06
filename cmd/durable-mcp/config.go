package main

import (
	"errors"
	"flag"

	"go.temporal.io/sdk/client"

	"github.com/nicogranuja/durable-mcp/internal/report"
)

// temporalConfig says which Temporal server, namespace, and task queue to
// use. The worker and the MCP server read it the same way, so that they
// agree by default.
type temporalConfig struct {
	address   string
	namespace string
	taskQueue string
}

// register defines the Temporal flags on fs. Each defaults to an
// environment variable, and then to a value that works with a local
// Temporal development server. verb says what the command does with the
// task queue, for the usage message.
func (c *temporalConfig) register(fs *flag.FlagSet, getenv func(string) string, verb string) {
	fs.StringVar(&c.address, "temporal-address", envOr(getenv, "TEMPORAL_ADDRESS", client.DefaultHostPort),
		"Temporal frontend `host:port` (env TEMPORAL_ADDRESS)")
	fs.StringVar(&c.namespace, "temporal-namespace", envOr(getenv, "TEMPORAL_NAMESPACE", client.DefaultNamespace),
		"Temporal `namespace` (env TEMPORAL_NAMESPACE)")
	fs.StringVar(&c.taskQueue, "task-queue", envOr(getenv, "DURABLE_MCP_TASK_QUEUE", report.DefaultTaskQueue),
		"Temporal task `queue` to "+verb+" (env DURABLE_MCP_TASK_QUEUE)")
}

// check reports a flag that must not be empty.
func (c *temporalConfig) check() error {
	switch {
	case c.address == "":
		return errors.New("-temporal-address must not be empty")
	case c.namespace == "":
		return errors.New("-temporal-namespace must not be empty")
	case c.taskQueue == "":
		return errors.New("-task-queue must not be empty")
	}
	return nil
}

// envOr returns the environment variable key, or fallback if it is unset
// or empty.
func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}
