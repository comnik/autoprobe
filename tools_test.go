package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected wrapped *exec.ExitError, got %T: %v", err, err)
	}
	if code := exitErr.ExitCode(); code != 7 {
		t.Fatalf("expected exit code 7, got %d", code)
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

// TestRunBash_WaitDelayUnblocksOnDetachedStdio pins the current backstop.
//
// When a backgrounded grandchild inherits the shell's stdio pipe and holds
// it open past the shell's own exit, runBash unblocks within WaitDelay
// (~5s) rather than waiting for the grandchild's full lifetime. Today this
// surfaces as an error ("exec: WaitDelay expired before I/O complete"),
// not a clean success — even though the shell exited 0.
func TestRunBash_WaitDelayUnblocksOnDetachedStdio(t *testing.T) {
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

	if err == nil {
		t.Fatalf("expected WaitDelay error, got nil")
	}
	if !strings.Contains(err.Error(), "WaitDelay") {
		t.Fatalf("expected error to mention WaitDelay, got %v", err)
	}
	// WaitDelay is 5s; allow slack. The hard contract: we never wait for
	// the grandchild's full 30s sleep.
	if elapsed > 10*time.Second {
		t.Fatalf("stdio backstop did not fire; runBash blocked for %v", elapsed)
	}
}

// ============================================================================
// Target-behavior tests — skipped until runBash is refactored toward the
// pi-mono shape. Each documents the contract we want next.
// ============================================================================

// TestRunBash_PluggableBackend: factor out a BashOperations-style interface
// so callers (and tests) can inject a non-shell executor. Mirrors pi-mono's
// createLocalBashOperations / BashOperations contract — enables future
// SSH/container backends and deterministic unit tests without spawning bash.
func TestRunBash_PluggableBackend(t *testing.T) {
	t.Skip("TODO: introduce BashOperations interface + constructor; until then, runBash hardcodes exec.CommandContext(\"bash\", ...) with no injection point")
}

// TestRunBash_OutputTruncationAndSpill: cap returned output at a
// line/byte budget; write the full output to a temp file and surface
// its path. Today CombinedOutput returns everything in memory, which
// is fine for `ls` but ruinous for `cat huge.log`.
func TestRunBash_OutputTruncationAndSpill(t *testing.T) {
	t.Skip("TODO: tail-truncate with full-output spill (cf. pi-mono OutputAccumulator + truncateTail)")
}

// TestRunBash_AbortViaContext: caller-driven cancellation, distinct from
// the configured timeout. Should return promptly with a cancellation
// error and the partial output collected so far. Today the only cancel
// path is the timeout context inside runBash.
func TestRunBash_AbortViaContext(t *testing.T) {
	t.Skip("TODO: accept a caller context (or AbortSignal-equivalent) so the TUI/agent can cancel a long-running command without waiting for the timeout")
}

// TestRunBash_TighterStdioHangHandling: when a detached descendant
// holds the stdio pipes open, return shortly after the shell itself
// exits — pi-mono uses EXIT_STDIO_GRACE_MS = 100, not the 5s WaitDelay
// window we currently fall back on.
func TestRunBash_TighterStdioHangHandling(t *testing.T) {
	t.Skip("TODO: replace WaitDelay backstop with proactive stdio-end + destroy (cf. pi-mono waitForChildProcess); should return within ~100ms of shell exit instead of ~5s")
}
