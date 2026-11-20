package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestMetricsAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"", "", false},
		{":9464", "127.0.0.1:9464", false}, // no host: loopback
		{"0.0.0.0:9464", "0.0.0.0:9464", false},
		{"[::1]:9464", "[::1]:9464", false},
		{"9464", "", true},
	}
	for _, tt := range tests {
		c := metricsConfig{addr: tt.in}
		err := c.check()
		if (err != nil) != tt.wantErr || (err == nil && c.addr != tt.want) {
			t.Errorf("check(%q) = %q, %v, want %q, error %v", tt.in, c.addr, err, tt.want, tt.wantErr)
		}
	}
}

func TestMetricsAddrFlags(t *testing.T) {
	t.Parallel()

	cfg, err := parseWorkerFlags([]string{"-metrics-addr", ":9464"}, env(nil), io.Discard)
	if err != nil || cfg.metrics.addr != "127.0.0.1:9464" {
		t.Errorf("worker -metrics-addr :9464 = %q, %v, want 127.0.0.1:9464", cfg.metrics.addr, err)
	}
	scfg, err := parseServeFlags(nil, env(map[string]string{envMetricsAddr: "127.0.0.1:9465"}), io.Discard)
	if err != nil || scfg.metrics.addr != "127.0.0.1:9465" {
		t.Errorf("serve %s = %q, %v, want 127.0.0.1:9465", envMetricsAddr, scfg.metrics.addr, err)
	}
	if _, err := parseServeFlags([]string{"-metrics-addr", "nope"}, env(nil), io.Discard); err == nil {
		t.Error("serve -metrics-addr nope did not fail")
	}
}

// startTelemetry serves /metrics on the address until stop, and nothing
// else there.
func TestStartTelemetryServesMetrics(t *testing.T) {
	t.Parallel()

	// Find a free port, then let startTelemetry listen on it.
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	tel, stop, err := startTelemetry(t.Context(), "serve", metricsConfig{addr: addr}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	counter, _ := tel.MeterProvider().Meter("test").Int64Counter("durable_mcp.test")
	counter.Add(t.Context(), 1)

	get := func(path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(metricsPath); code != http.StatusOK || !strings.Contains(body, `durable_mcp_test_total{otel_scope_name="test"`) {
		t.Errorf("GET %s = %d:\n%s", metricsPath, code, body)
	}
	if code, _ := get("/mcp"); code != http.StatusNotFound {
		t.Errorf("GET /mcp on the metrics address = %d, want 404", code)
	}

	stop()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+metricsPath, nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.Error("the metrics server still answers after stop")
	}
}
