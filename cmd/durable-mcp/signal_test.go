//go:build unix

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// envSignalHelper makes the test binary run signalHelper instead of the
// tests, in the child process of TestSecondSignalKills.
const envSignalHelper = "DURABLE_MCP_SIGNAL_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(envSignalHelper) == "1" {
		signalHelper()
		return
	}
	os.Exit(m.Run())
}

// signalHelper waits for a signal, then stands in for a graceful stop that
// takes much longer than the test waits.
func signalHelper() {
	ctx, stop := signalContext()
	fmt.Println("ready")
	<-ctx.Done()
	fmt.Println("draining")
	time.Sleep(time.Minute)
	stop()
	os.Exit(0)
}

func TestSecondSignalKills(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Env = append(os.Environ(), envSignalHelper+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	expect := func(want string) {
		t.Helper()
		if !lines.Scan() || lines.Text() != want {
			_ = cmd.Process.Kill()
			t.Fatalf("helper printed %q (%v), want %q", lines.Text(), lines.Err(), want)
		}
	}

	expect("ready")
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	expect("draining")

	// The helper stops catching signals just after it starts draining, so
	// repeat the second signal until it lands.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case err := <-exited:
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("helper exited with %v, want it killed by SIGINT", err)
			}
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
				t.Errorf("helper exited with %v, want it killed by SIGINT", err)
			}
			return
		case <-tick.C:
			_ = cmd.Process.Signal(syscall.SIGINT)
		case <-timeout:
			_ = cmd.Process.Kill()
			t.Fatal("a second SIGINT did not end the helper while it was draining")
		}
	}
}
