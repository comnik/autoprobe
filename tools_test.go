package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func callBash(t *testing.T, command string, timeoutMs int) (string, error) {
	t.Helper()
	args := map[string]any{"command": command}
	if timeoutMs > 0 {
		args["timeout"] = timeoutMs
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return runBash(raw)
}

// ============================================================================
// Characterization tests — pin what runBash already provides.
// ============================================================================

func TestRunBash_CapturesStdoutAndStderr(t *testing.T) {
	out, err := callBash(t, `echo out ; echo err 1>&2`, 5000)
	if err != nil {
		t.Fatalf("unexpected error: %v (output=%q)", err, out)
	}
	if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Fatalf("expected combined stdout+stderr, got %q", out)
	}
}

func TestRunBash_NonzeroExitReturnsErrorWithOutput(t *testing.T) {
	out, err := callBash(t, `echo before ; exit 7`, 5000)
	if err == nil {
		t.Fatalf("expected error for non-zero exit, got nil (output=%q)", out)
	}
	if !strings.Contains(out, "before") {
		t.Fatalf("expected returned output to include stdout written before exit, got %q", out)
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected wrapped *ExitError, got %T: %v", err, err)
	}
	if exitErr.Code != 7 {
		t.Fatalf("expected exit code 7, got %d", exitErr.Code)
	}
}

func TestRunBash_CustomTimeoutHonored(t *testing.T) {
	start := time.Now()
	_, err := callBash(t, `sleep 10`, 200)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected 'timed out' in error, got %v", err)
	}
	// 200ms timeout + SIGKILL on the pgroup; allow generous slack for CI.
	if elapsed > 3*time.Second {
		t.Fatalf("timeout fired too late: %v (configured 200ms)", elapsed)
	}
}

// TestRunBash_TimeoutKillsProcessGroup verifies that a child spawned by the
// shell is killed when the timeout fires — not just bash itself. Without
// Setpgid + negative-pid SIGKILL, a long-running grandchild would survive.
func TestRunBash_TimeoutKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group semantics are Unix-specific")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	cmd := fmt.Sprintf(`sleep 30 & echo $! > %s ; wait`, pidFile)

	_, err := callBash(t, cmd, 300)
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}

	deadline := time.Now().Add(3 * time.Second)
	var childPid int
	for time.Now().Before(deadline) {
		raw, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			trimmed := strings.TrimSpace(string(raw))
			if trimmed != "" {
				childPid, _ = strconv.Atoi(trimmed)
				if childPid > 0 {
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPid == 0 {
		t.Fatalf("child never wrote its pid to %s", pidFile)
	}
	t.Cleanup(func() {
		// Defensive: if the test ever regresses, don't leave sleep(30) lying around.
		_ = syscall.Kill(childPid, syscall.SIGKILL)
	})

	// Poll until init reaps the child. kill(pid, 0) returns ESRCH once gone.
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child pid %d still alive after timeout; process group kill did not reach it", childPid)
}

// TestRunBash_DetachedStdioReturnsPromptly: when a backgrounded grandchild
// inherits the shell's stdio pipe and holds it open past the shell's own
// exit, runBash returns within the stdio grace window (~100ms) and treats
// the shell's exit code as authoritative — no spurious WaitDelay error.
func TestRunBash_DetachedStdioReturnsPromptly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stdio inheritance semantics are Unix-specific")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// Subshell spawns sleep in background, records its pid, then exits.
	// The sleep inherits the outer bash's stdout pipe, so the read end on
	// the Go side stays open until WaitDelay fires.
	cmd := fmt.Sprintf(`( sleep 30 & echo $! > %s )`, pidFile)

	start := time.Now()
	_, err := callBash(t, cmd, 30_000)
	elapsed := time.Since(start)

	t.Cleanup(func() {
		raw, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			return
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	if err != nil {
		t.Fatalf("expected clean return despite detached grandchild holding stdio, got %v", err)
	}
	// Generous CI slack; pi-mono uses 100ms grace, so 1s is comfortable.
	if elapsed > time.Second {
		t.Fatalf("stdio grace window too slow: returned after %v", elapsed)
	}
}

// ============================================================================
// Target-behavior tests — skipped until runBash is refactored toward the
// pi-mono shape. Each documents the contract we want next.
// ============================================================================

// fakeBashOps lets tests stand in for the real executor.
type fakeBashOps struct {
	exec func(ctx context.Context, command string, onData func([]byte)) (int, error)
}

func (f *fakeBashOps) Exec(ctx context.Context, command string, onData func([]byte)) (int, error) {
	return f.exec(ctx, command, onData)
}

func TestRunBash_PluggableBackend_StreamsAndExitsCleanly(t *testing.T) {
	var capturedCommand string
	fake := &fakeBashOps{
		exec: func(_ context.Context, command string, onData func([]byte)) (int, error) {
			capturedCommand = command
			onData([]byte("hello "))
			onData([]byte("world"))
			return 0, nil
		},
	}
	raw, _ := json.Marshal(map[string]any{"command": "echo hi", "timeout": 5000})
	out, err := runBashWith(fake, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedCommand != "echo hi" {
		t.Fatalf("expected command 'echo hi', got %q", capturedCommand)
	}
	if out != "hello world" {
		t.Fatalf("expected accumulated output 'hello world', got %q", out)
	}
}

func TestRunBash_PluggableBackend_NonZeroExitYieldsExitError(t *testing.T) {
	fake := &fakeBashOps{
		exec: func(_ context.Context, _ string, onData func([]byte)) (int, error) {
			onData([]byte("partial output"))
			return 7, nil
		},
	}
	raw, _ := json.Marshal(map[string]any{"command": "x", "timeout": 5000})
	out, err := runBashWith(fake, raw)
	if err == nil {
		t.Fatalf("expected error for non-zero exit, got nil")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *ExitError, got %T: %v", err, err)
	}
	if exitErr.Code != 7 {
		t.Fatalf("expected exit code 7, got %d", exitErr.Code)
	}
	if !strings.Contains(out, "partial output") {
		t.Fatalf("expected partial output in result, got %q", out)
	}
}

func TestRunBash_PluggableBackend_SpawnErrorPropagates(t *testing.T) {
	sentinel := errors.New("spawn boom")
	fake := &fakeBashOps{
		exec: func(_ context.Context, _ string, _ func([]byte)) (int, error) {
			return -1, sentinel
		},
	}
	raw, _ := json.Marshal(map[string]any{"command": "x", "timeout": 5000})
	_, err := runBashWith(fake, raw)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error to propagate, got %v", err)
	}
}

func TestRunBash_PluggableBackend_TimeoutPropagatesViaContext(t *testing.T) {
	fake := &fakeBashOps{
		exec: func(ctx context.Context, _ string, _ func([]byte)) (int, error) {
			<-ctx.Done()
			return -1, ctx.Err()
		},
	}
	raw, _ := json.Marshal(map[string]any{"command": "sleep", "timeout": 100})
	start := time.Now()
	_, err := runBashWith(fake, raw)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timed-out error, got %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("timeout fired too late: %v", elapsed)
	}
}

func TestRunBash_OutputTruncationAndSpill(t *testing.T) {
	// ~100KB total, well over the 64KB default threshold. Each chunk is a
	// numbered line so we can identify the tail unambiguously.
	const nLines = 1000
	var emitted strings.Builder
	for i := 0; i < nLines; i++ {
		fmt.Fprintf(&emitted, "line %04d %s\n", i, strings.Repeat("x", 95))
	}
	fake := &fakeBashOps{
		exec: func(_ context.Context, _ string, onData func([]byte)) (int, error) {
			// Emit in modest chunks so the rolling-window/spill paths both exercise.
			data := []byte(emitted.String())
			for i := 0; i < len(data); i += 4096 {
				end := i + 4096
				if end > len(data) {
					end = len(data)
				}
				onData(data[i:end])
			}
			return 0, nil
		},
	}
	raw, _ := json.Marshal(map[string]any{"command": "x", "timeout": 5000})
	out, err := runBashWith(fake, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(out) >= emitted.Len() {
		t.Fatalf("expected truncated output, got %d bytes (emitted %d)", len(out), emitted.Len())
	}
	if !strings.Contains(out, "Output truncated") {
		t.Fatalf("expected truncation marker, got tail %q", out[max(0, len(out)-200):])
	}
	// Tail should include the last emitted lines; head should not.
	if !strings.Contains(out, "line 0999") {
		t.Fatalf("expected last line in tail, got %q", out[max(0, len(out)-300):])
	}
	if strings.Contains(out, "line 0000\n") {
		t.Fatalf("did not expect first line in truncated tail")
	}

	// Parse the temp path out of the marker and confirm the full bytes spilled.
	re := regexp.MustCompile(`Full output: (\S+)\]`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("expected 'Full output: <path>' in marker, got tail %q", out[max(0, len(out)-200):])
	}
	tempPath := m[1]
	t.Cleanup(func() { _ = os.Remove(tempPath) })

	data, err := os.ReadFile(tempPath)
	if err != nil {
		t.Fatalf("reading spill file %s: %v", tempPath, err)
	}
	if len(data) != emitted.Len() {
		t.Fatalf("spill file size %d != emitted %d", len(data), emitted.Len())
	}
	if string(data) != emitted.String() {
		t.Fatalf("spill file content differs from emitted bytes")
	}
}

func TestRunBash_OutputNotTruncatedBelowThreshold(t *testing.T) {
	payload := "small output\n"
	fake := &fakeBashOps{
		exec: func(_ context.Context, _ string, onData func([]byte)) (int, error) {
			onData([]byte(payload))
			return 0, nil
		},
	}
	raw, _ := json.Marshal(map[string]any{"command": "x", "timeout": 5000})
	out, err := runBashWith(fake, raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != payload {
		t.Fatalf("expected output passed through verbatim, got %q", out)
	}
	if strings.Contains(out, "truncated") {
		t.Fatalf("did not expect truncation marker for sub-threshold output")
	}
}

